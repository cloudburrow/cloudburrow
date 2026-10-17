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
| Pub/Sub | 3 | 0 | 1 | 1 | 1 |
| Cloud Tasks | 0 | 0 | 0 | 0 | 0 |
| Cloud Run | 2 | 0 | 1 | 0 | 1 |
| Cloud Scheduler | 1 | 0 | 0 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 5 | 0 | 1 | 3 | 1 |
| Secret Manager | 1 | 0 | 0 | 0 | 1 |
| Resource Manager | 0 | 0 | 0 | 0 | 0 |
| Vertex AI | 1 | 0 | 1 | 0 | 0 |
| BigQuery | 31 | 12 | 7 | 12 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 4 | 0 | 3 | 0 | 1 |
| Cloud SQL, Memorystore | 3 | 2 | 1 | 0 | 0 |
| Functions, credentials, tooling | 0 | 0 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **54** | **14** | **16** | **18** | **6** |

## Cloud Storage

Closed: `objects.bulkRestore` (#1279), `buckets.operations.list` filters (#1280), `channels.stop`
and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up`
instance (#1290), the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), the gRPC
`google.storage.v2` API (#1321), and its customer-supplied encryption keys, ACLs, managed-folder
IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter` (#1345). ACLs
and object IAM on the JSON API are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Notifications: the server with no Pub/Sub refuses a configuration 501, as it has no topic to verify or deliver to (#1444). Google documents no error for a topic that does not exist (reporting-changes says only to create the topic first; gcloud and Terraform create it), so there is no documented refusal to answer instead; the 501 names the reason (#1505) (#1521) | no emulator support (needs Pub/Sub) | S |
| UNVERIFIED error codes (a non-empty bucket delete, a repeat retention lock, managed-folder and folder refusals, batch limits, CORS on JSON endpoints and others; [storage.md](coverage/storage.md)) (#1445) | not documented by Google | S |

## Pub/Sub

Closed: `DetachSubscription`, `UpdateSnapshot` and Protocol Buffer schemas, served by CloudBurrow's
Pub/Sub front (#1406), and IAM on topics, subscriptions, snapshots and schemas, stored and never
enforced (#1367).

| Gap | Why open | Size |
|---|---|---|
| The front's messages for a refused `.proto` definition or message, and for the calls Google documents no answer to on a detached subscription (`FAILED_PRECONDITION` for a snapshot of it or a seek to a snapshot, `INVALID_ARGUMENT` for an immutable field), are its own (#1445; built in #1446) | needs live GCP: Google's messages not measured | S |
| UNVERIFIED error codes: subscription expiration, exactly-once and label refusals ([pubsub.md](coverage/pubsub.md); an invalid label's message is #982) (#1445) | not documented by Google | S |

| Delivery to an App Engine app (#1447) (#1506): with `--appengine-url` a local server stands in for the app and gets each attempt as `AppEngineHttpTarget` documents it (built in #1506), else every attempt fails `NOT_FOUND` (HTTP 404) (#1521) | no emulator support (no App Engine runtime runs locally, and Google documents no mapping to Cloud Run, so the stand-in is the developer's own server) | — |
## Cloud Tasks

Closed: `RunTask`, `BatchCreateTasks`, `BatchDeleteTasks` and the CMEK config (#1215), `oidcToken`
/ `oauthToken` on HTTP tasks and Cloud Scheduler jobs (#1243), `ListQueues` filters on any Queue
field (#1255), `stackdriver_logging_config`, with each sampled dispatch written to Cloud Logging
(#1256), and a queue-level `http_target` (#1282). Queue IAM conditions are excluded (below).

| Gap | Why open | Size |
|---|---|---|

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
| Cloud Build: triggers with a repository source or a `filename` are refused 501 with the reason (they need a hosted git service: no Cloud Source Repositories, Secure Source Manager or Developer Connect is served, and Google's repository sources name hosted services, not local paths); Pub/Sub triggers are built (#1506). Artifact Registry: Maven, npm, Python, Apt and Yum methods (8) `UNIMPLEMENTED` with the reason, as only `DOCKER` repositories are served; remote and virtual repositories need upstream registries. Built in #1449: `dynamicSubstitutions`, `secretEnv` on docker and pack steps, `ApproveBuild` and `approvalRequired`, and 19 Artifact Registry methods (rules, attachments, project and VPC-SC settings, package, version and file updates, `BatchDeleteVersions`, `ExportArtifact`; [artifactregistry.md](coverage/artifactregistry.md)) (#1449) (#1506) (#1521) | no emulator support: built; the rest is refused with its reason (a hosted git service, or package formats not served) | M |

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
| A BigQuery sink's `export_errors` column types and `schemaErrorDetail` text, `Any` fields but `serviceData` (left out), and the sink error codes but `topic_not_found` are CloudBurrow's (#1451; `export_errors` and `protoPayload` are built; `Duration` fields are `FLOAT` seconds, as Google's Log Analytics migration guide shows for `httpRequest.latency`, #1506) (#1521) | not documented by Google (the export page lists the `export_errors` columns without types, and `schemaErrorDetail` only as BigQuery's message); needs a live sink | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by the sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| Single-tenant HSM (#1469): `upgrade_key_trust` proposals and import jobs at HSM_SINGLE_TENANT are UNIMPLEMENTED, as trusted key wrapping is; instances and proposals never pass through their transitional states, since nothing is provisioned (HsmManagement and HSM_SINGLE_TENANT keys are served since #1469; HSM since #1452, without an HSM or an attestation; EXTERNAL and EXTERNAL_VPC since #1469, the instance acting as the EKM) (#1506) (#1521) | no emulator support | M |
| REST: `alt` other than JSON, and `fields`/`$fields`, are `UNIMPLEMENTED` (#1445) | not documented by Google | S |
| UNVERIFIED error codes ([kms.md](coverage/kms.md)) (#1445) | not documented by Google | S |
| Project-number parents are refused; whether Google accepts them, and whether an empty key ring can be deleted (#1445) | needs live GCP | S |
| `key_access_justifications_policy` is enforced (#1506): encrypt, decrypt and sign fail under a policy with no reasons, as documented, or without `CUSTOMER_INITIATED_ACCESS`, a local call's reason; the refusal's code and text (`FAILED_PRECONDITION`) are CloudBurrow's, and project and organization default policies are not served (#1452) (#1521) | not documented by Google | S |

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

No gap is left open here.

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
| `ALTER TABLE` is 501 for a foreign key to another project's table (an instance serves one project), the table options not built, `ALTER COLUMN SET OPTIONS (data_policies, data_governance_tags)`, an `ALTER TABLE` in a dry run of a script (one in a script is carried out in its turn, #1459), and legacy SQL; a statement of several actions is carried out a step at a time, so a later refusal leaves the earlier steps done; a nested field's `ALTER COLUMN SET DEFAULT`, and a `WRITE_TRUNCATE_DATA` query job whose result lacks a column with a default, are 501 (#1456) | not built yet | S |
| A dry run of `CREATE VIEW` with a column list, without `OR REPLACE` or `IF NOT EXISTS`, of a view that exists is 501, as what BigQuery's dry run answers for it is not known (#1456); so is a STRUCT field's `data_policies`, as Google documents data policies on a column only (#1456); smart tuning's rewrite of a base-table query to read a materialized view is not built: the results are the same, and whether BigQuery chooses a view is its cost estimate (`rejectedReason` `COST`), which Google does not document (#1456) | not documented by Google | S |
| What #1426, #1455 and #1482 leave of `privacy_policy`: a read of an aggregation threshold or join restriction table or view in a subquery, CTE, another view, a script or a non-INNER join is 501; of differential privacy (#1455, #1482 built: `HAVING`, subqueries, CTEs, scripts, `PUBLIC_GROUPS`), `COUNT(DISTINCT)` and `max_rows_contributed` (documented by neither BigQuery nor GoogleSQL) and a DP aggregate inside an expression are 501 (#1502) | not built yet | L |
| What #1503 leaves of change history: a table with a column named `_CHANGE_IS_FOR_UPDATE` (the change table's own name, which BigQuery does not reserve) keeps no history, and `ALTER SCHEMA … SET OPTIONS` in a multi-statement query is 501 (#1522); every table's rows are recorded twice, whatever its options (#1466) | not built yet | S |
| What #1426 leaves of `privacy_policy`: differential privacy (`SELECT WITH DIFFERENTIAL_PRIVACY`, a `differential_privacy_policy`) is 501, no noise mechanism or epsilon/delta budget (#1455); a read of a policy table or view in a subquery, CTE, another view, a script or a non-INNER join is 501 (#1502) | not built yet | L |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name, a Parquet name with a period) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (JobConfigurationLoad read again 2026-10-17: V1 "Support alphanumeric + underscore characters and names must start with a letter or underscore", V2 "Support flexible column name", neither saying how a name still refused or two the same are named) (#1457) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting (into a `JSON` column the schema gives, mixed values load since #1457), `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501; schema-detect read again 2026-10-17 (a sample of "up to the first 500 rows", the header rule, generic names "such as string_field_1") documents no more of these (#1457) | not documented by Google | S |
| CSV: a quote outside ISO-8859-1, a delimiter of several bytes or outside ASCII in UTF-16 or UTF-32 data (the reference converts a quote or delimiter "to ISO-8859-1" and splits on "the first byte", and says nothing of other encodings), a `maxBadRecords` load the emulator still fails on a value the front passes (a number written as a fraction into an INTEGER, a DATETIME or TIMESTAMP), `sourceColumnMatch` `NAME` with no row skipped (ISO-8859-1 delimiters and quotes, a quote's first byte, `skipLeadingRows` N and the header with `autodetect`, `NAME` after skipped rows, and, in #1457's second part, a delimiter's first byte, a delimiter or quote outside ASCII in UTF-8 data and an empty `STRING` with a `nullMarker` and, under `maxBadRecords`, a BOOL, DATE, TIME or number its column refuses as a bad record are built); JSON: a null field outside the schema without `ignoreUnknownValues` (the reference does not say whether a null is an "extra value"; under `ignoreUnknownValues` it is ignored since #1457), a `maxBadRecords` load the emulator still fails on a value the front passes (a value its column refuses is a bad record since #1457) | no emulator support | M |
| Parquet: the refused columns left (an INT96 with a part of a microsecond, a DECIMAL that would round, DECIMAL to STRING, a NULL element or a LIST of lists with list inference); a NaN loads and unlisted types are 400 since #1457; the Parquet loading page (read again 2026-10-17) maps LIST, ENUM, DECIMAL and INT96 but says nothing of these values, so they stay 501 (#1457) | not documented by Google | S |
| What #1426, #1455, #1482 and #1502 leave of `privacy_policy` (#1502 built: `SELECT WITH AGGREGATION_THRESHOLD` in a subquery, a CTE, after a `WITH` list and in `CREATE TABLE … AS`, scripts a statement at a time, joins that are not INNER by each `join_condition`'s wording): a read of an aggregation threshold or join restriction table or view in a subquery or CTE outside its own `SELECT WITH AGGREGATION_THRESHOLD`, or through another view, two clauses in one query, `JOIN_ANY` met only by an outer join, `JOIN_ALL` alongside one, a `NATURAL` join; of differential privacy, `COUNT(DISTINCT)`, `max_rows_contributed` and a DP aggregate inside an expression are 501: documented by neither BigQuery nor GoogleSQL (#1517) | not documented by Google | M |
| What #1418 leaves of change history: an ALTER TABLE that rebuilds the table and CREATE OR REPLACE TABLE start the history over; CHANGES in a multi-statement transaction and on a change-data-capture table are not refused; every table's rows are recorded twice, whatever its options (#1466) (#1503) | not built yet | M |
| What #1466 leaves of change history: `CHANGES` of a table that had a multi-statement transaction committed to it within the range is not refused; changes are kept seven days whatever the dataset's time travel window (only the window's are read) (#1503) | not built yet | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name, a Parquet name with a period) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (#1457) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting (into a `JSON` column the schema gives, mixed values load since #1457), `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501 (#1457) | not documented by Google | S |
| CSV: a quote outside ISO-8859-1, a delimiter of several bytes or outside ASCII in UTF-16 or UTF-32 data, a `maxBadRecords` load the emulator still fails on a value the front passes (a number written as a fraction into an INTEGER, a DATETIME or TIMESTAMP), `sourceColumnMatch` `NAME` with no row skipped (ISO-8859-1 delimiters and quotes, a quote's first byte, `skipLeadingRows` N and the header with `autodetect`, `NAME` after skipped rows, and, in #1457's second part, a delimiter's first byte, a delimiter or quote outside ASCII in UTF-8 data and an empty `STRING` with a `nullMarker` and, under `maxBadRecords`, a BOOL, DATE, TIME or number its column refuses as a bad record are built); JSON: a null field outside the schema (the reference does not say whether a null is an "extra value"), a `maxBadRecords` load the emulator still fails on a value the front passes (a value its column refuses is a bad record since #1457) | no emulator support | M |
| Parquet: the refused columns left (an INT96 with a part of a microsecond, a DECIMAL that would round, DECIMAL to STRING, a NULL element or a LIST of lists with list inference); a NaN loads and unlisted types are 400 since #1457 | not documented by Google | S |
| A load of several Cloud Storage objects when the front was given no Cloud Storage (#1457) | no emulator support | S |
| Partition decorators: `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination, a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, and through a partition a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order or a RECORD's fields changed, are 501, none documented by BigQuery (docs read again 2026-10-16, and Google's docs are silent on each, so they stay 501: streaming to a decorator is documented for daily ingestion-time tables only, and schemaUpdateOptions name no rule for these) (#1458) | not documented by Google | S |
| `FOR SYSTEM_TIME AS OF` a past time of an ingestion-time partitioned table in a snapshot, clone or copy of `table@time` (a copy keeps no `_PARTITIONTIME`), or in a query whose select list has a `*` and that names `_PARTITIONTIME` or `_PARTITIONDATE` (#1458; queries otherwise read since #1458) (#1483) | not built yet | M |
| What #1483 leaves of undelete: an external table, and a function made by `routines.insert` before the front started, are not kept by a deletion; `UNDROP SCHEMA` in a script with control flow, or whose other statements name the dataset, and in a dry run of a script (#1483) (#1503) (#1522) | not built yet | M |
| What #1483 leaves of undelete: `UNDROP SCHEMA` in a script with control flow, or whose other statements name the dataset, and in a dry run of a script (#1483) (#1503) | not built yet | M |
| BQML: `LINEAR_REG`, binary and multiclass `LOGISTIC_REG`, `KMEANS` and `PCA` are trained by the front, with `TRANSFORM`, ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO, ML.TRAINING_INFO, ML.CONFUSION_MATRIX, ML.ROC_CURVE, ML.CENTROIDS, ML.PRINCIPAL_COMPONENTS, ML.PRINCIPAL_COMPONENT_INFO and models.* (#1465, #1481); every other model type, remote models, ML preprocessing functions in TRANSFORM and the other ML functions are 501 with the reason (#1504) | no emulator support: training Google does not document, or Google's infrastructure | L |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type (#1445) | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) (#1445) | not documented by Google | S |
| Table options: `storage_uri` (#1427) is tracked on its own, and what #1426 and #1418 leave in the rows below (#1456) | not built yet | M |
| Project configurations: `default_kms_key_name` reaches `tables.insert`, a lone `CREATE TABLE` and a query, copy or JSON load job's new destination table, not a table a script makes or an upload's load; `ALTER PROJECT` / `ALTER ORGANIZATION` inside a block, `IF` or loop of a script is carried out in its turn (#1459), and 501 only in a dry run; `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows, which Google does not document (#1456) | not built yet | S |
| A table made before the engine had the stream-time column (googlesqlite patch 0030; `CREATE TABLE … AS SELECT` has it since patch 0056, #1458; a temporary table cannot be streamed to) keeps no stream time, so its streamed rows are held from DML only while in the 20-second streaming buffer; a table restored from state saved before #1277 keeps no ingestion time (#1458) | no emulator support (the times were never recorded) | S |
| `PARTITIONS`' `last_modified_time` is NULL where the engine's change table holds no write (seven days kept), and for an ingestion-time partitioned table whose change table was made before googlesqlite patch 0050 (#1458); region `TABLE_STORAGE` has no physical byte columns, and an older table than 90 days no active or long-term split (#1458) | no emulator support (no compressed sizes; write times kept seven days) | S |
| Avro loads (built in #1457): a union of several types, a `map`, a recursive record, an array whose items may be null, a `decimal` made STRING by `decimalTargetTypes` and, for Avro and Parquet, a data file whose schema differs from its `referenceFileSchemaUri`'s (one of the reference's schema loads since #1457) are 501, as BigQuery does not document how it loads them; zstandard files are 501, as goavro reads null, deflate and snappy only (#1457) | not documented by Google | S |
| `ORC` loads (no Go module in go.mod reads ORC), `DATASTORE_BACKUP` loads, `hivePartitioningOptions`, `jsonExtension` `GEOJSON` and a manifest `fileSetSpecType` are 501 (#1457) | no emulator support | M |
| What #1503 leaves of change history: a table whose columns are named `_CHANGE_…` keeps no history; `ALTER SCHEMA … SET OPTIONS (max_time_travel_hours = …)` changes the window without letting go of the history outside the old one first (the next `APPENDS` or `CHANGES` lets go of what is outside the new one); every table's rows are recorded twice, whatever its options (#1466) (#1522) | not built yet | S |

## Spanner, Bigtable, Firestore, Datastore

Closed: Datastore structured aggregations and Firestore `PartitionQuery` (#1405).

| Gap | Why open | Size |
|---|---|---|
| Whether production reorders an array's excluded values after its indexed ones, as the emulator does (#941) | needs live GCP | S |
| Spanner: 3 RPCs refused, each naming why: `AddSplitPoints` (the emulator does not split databases), `InternalUpdateGraphOperation` (Google's internal method) and `FetchCacheUpdate` (location-aware routing, with one server to route to; [spanner.md](coverage/spanner.md)). Backups and restores, backup schedules, instance configurations (Google's and custom), `MoveInstance`, partition updates, database roles, the operation listings and IAM on backups and roles are served by the call front since #1498 (#1498) (#1523) | no emulator support: the emulator is one server and does not split databases | S |
| Bigtable: 6 RPCs refused (the change streams, which need a change log nothing keeps, and the session data plane: `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`, `OpenMaterializedView`; [bigtable.md](coverage/bigtable.md)); SQL runs GoogleSQL for Bigtable's `SELECT` with views, with UNPACK, window functions and some documented functions refused (#1461) (#1499) (#1511) (#1523) | no emulator support | M |
| Firestore: `ExecutePipeline`, and the Firestore Admin user credentials (8 methods, each naming why; [firestoreadmin.md](coverage/firestoreadmin.md)); backups, restore, clone, `ExportDocuments`, `ImportDocuments` and `BulkDeleteDocuments` are served since #1500, and Datastore `explain_options` (#1500) (#1523) | no emulator support: pipelines and user credentials are Enterprise edition features (MongoDB compatibility), and the emulator is the Standard edition | L |

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
| Cloud SQL: a PostgreSQL external primary (Database Migration Service's pglogical), a custom import from `dumpFilePath`, `OFFLINE` and `PHYSICAL` syncs, and `demote`/`demoteMaster` (the reverse direction) stay 501 (a MySQL external primary, its source representation instance, `verifyExternalSyncSettings` and `startExternalSync` are built since #1501); `MYSQL_5_6`, `MYSQL_5_7` and SQL Server versions, and `BAK` and `TDE` files (their images, `mysql:5.6`, `mysql:5.7` and `mcr.microsoft.com/mssql/server`, are linux/amd64 only, checked 2026-10-17); upgrading a PostgreSQL replica on its own (streaming replication does not cross major versions); high availability, read replicas and point-in-time recovery on PostgreSQL 9.6 to 11 (no `pg_promote()` or `recovery.signal`), each 501 naming why (PostgreSQL 9.6 to 12, a MySQL replica's own upgrade, Cloud SQL's 400 for a legacy failover replica and for a PostgreSQL database's charset change are built since #1462); 22 methods 501, each naming what is missing (blue-green deployments and `connect.resolve`, server and Entra ID certificates, `demote`, `switchover`, `reencrypt`, disk shrink and others; [sqladmin.md](coverage/sqladmin.md)) (#1462) (#1501) (#1524) | not built yet | L |
| Backup and DR: workload types other than Cloud SQL instances (Compute Engine instances and disks, AlloyDB, Filestore, which CloudBurrow does not run), management servers and the appliance protocol, trials, resource backup configs' `fetch` (every resource type, through Cloud Asset Inventory) and `serviceConfig.initialize` (22 methods 501, each naming what is missing; `encryptionConfig` with this instance's Cloud KMS and `resourceBackupConfigs.list` of Cloud SQL instances are built since #1501; [backupdr.md](coverage/backupdr.md)) (#1462) (#1501) (#1524) | not built yet | L |

## Functions, credentials, tooling

Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319);
Cloud Functions v2 and v1 APIs (#1377, #1428); Pub/Sub and project IAM in Terraform (#1367,
#1368).

| Gap | Why open | Size |
|---|---|---|
| Terraform resources beyond the verified set (#1304): the other `google_spanner_*` resources (instance configs, backup schedules), a worker pool's `scaling_mode` (Google publishes no such field), `google_logging_folder_settings` (refused, as Google documents), `google_organization_iam_policy` (authoritative over the shared organization), `google_sql_source_representation_instance` (refused 501), `google_folder_organization_policy` and `google_organization_policy` (folder and organization org policies are refused 501), `google_secret_manager_regional_secret` and `google_bigquery_data_transfer_config` (their endpoints are not set: not served), the `google_storage_buckets`, `google_projects` and `google_folders` data sources (the provider calls Google's URLs whatever its custom endpoints say, so `cloudburrow terraform` refuses them), the resources whose API refuses them (Bigtable authorized views, log bucket CMEK; audit configs, tag IAM, Bigtable table IAM and Spanner IAM are stored since #1486), and the other resources of the served services | partly verified (the ten batches of #1304 and the verified rows of compatibility.md) | L |

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
