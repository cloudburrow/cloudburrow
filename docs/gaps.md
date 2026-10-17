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
| Cloud Storage | 5 | 0 | 4 | 1 | 0 |
| Pub/Sub | 2 | 0 | 0 | 1 | 1 |
| Cloud Tasks | 1 | 0 | 1 | 0 | 0 |
| Cloud Run | 2 | 0 | 1 | 0 | 1 |
| Cloud Scheduler | 2 | 0 | 1 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 6 | 0 | 3 | 2 | 1 |
| Secret Manager | 1 | 0 | 0 | 0 | 1 |
| Resource Manager | 0 | 0 | 0 | 0 | 0 |
| Vertex AI | 1 | 0 | 1 | 0 | 0 |
| BigQuery | 29 | 9 | 12 | 8 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 5 | 0 | 4 | 0 | 1 |
| Cloud SQL, Memorystore | 3 | 2 | 1 | 0 | 0 |
| Functions, credentials, tooling | 2 | 2 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **61** | **13** | **29** | **13** | **6** |

## Cloud Storage

Closed: `objects.bulkRestore` (#1279), `buckets.operations.list` filters (#1280), `channels.stop`
and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up`
instance (#1290), the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), the gRPC
`google.storage.v2` API (#1321), and its customer-supplied encryption keys, ACLs, managed-folder
IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter` (#1345). ACLs
and object IAM on the JSON API are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| gRPC `google.storage.v2`: `Bucket.ip_filter` stays `UNIMPLEMENTED` (and `ipFilter` 400 on the JSON API): enforcing it needs the caller's VPC network and the `storage.buckets.exemptFromIpFilter` permission, which are not modelled, and a filter stored without effect would look like a control that is not there. Appendable objects, `custom_placement_config` and `SetIamPolicy`'s `update_mask` are built (#1443) | no emulator support: enforcement needs what is not modelled | S |
| `managedFolders.update`: its only settable field is `rapidCacheConfig`, and an insert carrying it is 501 (#1444) | no emulator support (no cache tier) | S |
| `rapidCaches.*` (#1444; Anywhere Caches, as records with no cache tier, `buckets.relocate`, `advanceRelocateBucket` and `operations.cancel` are built) | no emulator support (no cache tier) | M |
| Notifications: the server with no Pub/Sub refuses a configuration 501, as it has no topic to verify or deliver to (#1444) | no emulator support (needs Pub/Sub) | S |
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
| Delivery to an App Engine target (#1447) | no emulator support (no App Engine): tasks and `app_engine_routing_override` are stored as documented and every attempt fails `NOT_FOUND` (HTTP 404), since no App Engine runs locally and Google documents no mapping to Cloud Run | — |

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
| Cloud Build: triggers with a repository or Pub/Sub source or a `filename` are refused 501 with the reason (they need a hosted git service or a Google-owned subscription). Artifact Registry: Maven, npm, Python, Apt and Yum methods (8) `UNIMPLEMENTED` with the reason, as only `DOCKER` repositories are served; remote and virtual repositories need upstream registries. Built in #1449: `dynamicSubstitutions`, `secretEnv` on docker and pack steps, `ApproveBuild` and `approvalRequired`, and 19 Artifact Registry methods (rules, attachments, project and VPC-SC settings, package, version and file updates, `BatchDeleteVersions`, `ExportArtifact`; [artifactregistry.md](coverage/artifactregistry.md)) (#1449) | no emulator support: built; the rest is refused with its reason (a hosted git service, a Google-owned subscription, or package formats not served) | M |
| A stale `etag` on a job is `ABORTED`; a failed `RunJob` is `FAILED_PRECONDITION`; `CancelExecution` of an ended execution is `FAILED_PRECONDITION` (#1445) | needs live GCP | S |

## Cloud Scheduler

Closed by the sweep: Locations (#1237).

| Gap | Why open | Size |
|---|---|---|
| Delivery to an App Engine target (#1447) | no emulator support (no App Engine): jobs are stored as documented and every attempt fails `NOT_FOUND` (HTTP 404), since no App Engine runs locally and Google documents no mapping to Cloud Run | — |
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
| A BigQuery sink's `export_errors` column types and `schemaErrorDetail` text, the types of `httpRequest.latency` and other `Duration` and `Any` fields (left out), and the sink error codes but `topic_not_found` are CloudBurrow's (#1451; `export_errors` and `protoPayload` are built) | not documented by Google; needs a live sink | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by the sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| EXTERNAL, EXTERNAL_VPC and HSM_SINGLE_TENANT protection levels, `crypto_key_backend`, `external_protection_level_options` (#1469; HSM is served since #1452, without an HSM or an attestation; no open-source key manager speaks Google's Cloud EKM protocol, and Google publishes none, checked 2026-10-16) | no emulator support | L |
| HsmManagement and `EkmService/VerifyConnectivity` (#1469; EkmService CRUD, Autokey and AutokeyAdmin are served since #1452, and IAM on `ekmConfig` and `ekmConnections`, stored and not enforced, since #1469) | no emulator support | L |
| `key_access_justifications_policy` is stored and returned, not enforced (#1452): no call carries an access justification locally | no emulator support | S |
| REST: `alt` other than JSON, and `fields`/`$fields`, are `UNIMPLEMENTED` (#1445) | not documented by Google | S |
| UNVERIFIED error codes ([kms.md](coverage/kms.md)) (#1445) | not documented by Google | S |
| Project-number parents are refused; whether Google accepts them, and whether an empty key ring can be deleted (#1445) | needs live GCP | S |

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
| Multimodal `EmbedContent` and `:predict` (#1454) are served from a local CLIP model (text and images, inline or `gs://`), but `TestMultimodalEmbeddingsThroughThePredictionClient` and the Embeddings screen's image test have not been run: they need a `-tags onnx` build with ONNX Runtime ≥ 1.29; video, audio and PDF parts, and 1408 dimensions, are `UNIMPLEMENTED` (CLIP has no such encoders) | not yet run against the official SDK; no local video, audio or PDF encoder | M |

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
| `ALTER TABLE` is 501 for a foreign key to another project's table (an instance serves one project), the table options not built, `ALTER COLUMN SET OPTIONS (data_policies, data_governance_tags)`, an `ALTER TABLE` in a dry run of a script (one in a script with a `TEMP` table or function is carried out in its turn, #1459), and legacy SQL; a statement of several actions is carried out a step at a time, so a later refusal leaves the earlier steps done; a nested field's `ALTER COLUMN SET DEFAULT`, and a `WRITE_TRUNCATE_DATA` query job whose result lacks a column with a default, are 501 (#1456) | not built yet | S |
| A dry run of `CREATE VIEW` with a column list, without `OR REPLACE` or `IF NOT EXISTS`, of a view that exists is 501, as what BigQuery's dry run answers for it is not known (#1456) | not documented by Google | S |
| Table options and materialized views: `ALTER COLUMN … SET OPTIONS (rounding_mode)` is 501 where the Table OPTIONS row says so; `require_partition_filter` checks that the column is named in a `WHERE`, not that it prunes; smart tuning's rewrite of a base-table query to read a materialized view is not built (#1456); `storage_uri` (#1427) is tracked on its own, and what #1426 and #1418 leave in the rows below | not built yet | M |
| What #1426, #1455 and #1482 leave of `privacy_policy`: a read of an aggregation threshold or join restriction table or view in a subquery, CTE, another view, a script or a non-INNER join is 501; of differential privacy (#1455, #1482 built: `HAVING`, subqueries, CTEs, scripts, `PUBLIC_GROUPS`), `COUNT(DISTINCT)` and `max_rows_contributed` (documented by neither BigQuery nor GoogleSQL) and a DP aggregate inside an expression are 501 | not built yet | L |
| What #1418 leaves of change history: an ALTER TABLE that rebuilds the table and CREATE OR REPLACE TABLE start the history over; CHANGES in a multi-statement transaction and on a change-data-capture table are not refused; every table's rows are recorded twice, whatever its options (#1466) | not built yet | M |
| Project configurations: `default_time_zone` does not reach a string literal the analyzer coerces to a `TIMESTAMP` (`ts > '2020-01-01 00:00:00'` reads it in UTC); `default_kms_key_name` reaches `tables.insert` and a lone `CREATE TABLE`, not a table a script or a query, load or copy job makes; `ALTER PROJECT` / `ALTER ORGANIZATION` inside a block, `IF` or loop of a script is carried out in its turn (#1459), and 501 only in a dry run; `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows, which Google does not document (#1456) | not built yet | S |
| What #1426 leaves of `privacy_policy`: differential privacy (`SELECT WITH DIFFERENTIAL_PRIVACY`, a `differential_privacy_policy`) is 501, no noise mechanism or epsilon/delta budget (#1455); a read of a policy table or view in a subquery, CTE, another view, a script or a non-INNER join is 501 | not built yet | L |
| What #1466 leaves of change history: `CHANGES` of a table that had a multi-statement transaction committed to it within the range is not refused; changes are kept seven days whatever the dataset's time travel window (only the window's are read) | not built yet | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name, a Parquet name with a period) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (#1457) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting (into a `JSON` column the schema gives, mixed values load since #1457), `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501 (#1457) | not documented by Google | S |
| CSV: a quote outside ISO-8859-1, a delimiter of several bytes or outside ASCII in UTF-16 or UTF-32 data, a `maxBadRecords` load the emulator still fails on a value the front passes (a number written as a fraction into an INTEGER, a DATETIME or TIMESTAMP), `sourceColumnMatch` `NAME` with no row skipped (ISO-8859-1 delimiters and quotes, a quote's first byte, `skipLeadingRows` N and the header with `autodetect`, `NAME` after skipped rows, and, in #1457's second part, a delimiter's first byte, a delimiter or quote outside ASCII in UTF-8 data and an empty `STRING` with a `nullMarker` and, under `maxBadRecords`, a BOOL, DATE, TIME or number its column refuses as a bad record are built); JSON: a null field outside the schema (the reference does not say whether a null is an "extra value"), a `maxBadRecords` load the emulator still fails on a value the front passes (a value its column refuses is a bad record since #1457) | no emulator support | M |
| Parquet: the refused columns left (an INT96 with a part of a microsecond, a DECIMAL that would round, DECIMAL to STRING, a NULL element or a LIST of lists with list inference); a NaN loads and unlisted types are 400 since #1457 | not documented by Google | S |
| A load of several Cloud Storage objects when the front was given no Cloud Storage (#1457) | no emulator support | S |
| Partition decorators: `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination, a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, and through a partition a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order or a RECORD's fields changed, are 501, none documented by BigQuery (docs read again 2026-10-16: streaming to a decorator is documented for daily ingestion-time tables only, and schemaUpdateOptions name no rule for these) (#1458) | not documented by Google | S |
| A table made by `CREATE TABLE … AS SELECT`, a temporary table, or one made before the engine had the column keeps no stream time, so its streamed rows are held from DML only while in the 20-second streaming buffer; a table restored from state saved before #1277 keeps no ingestion time (#1458) | no emulator support | S |
| `PARTITIONS`' `last_modified_time` is NULL for an ingestion-time partitioned table's partitions and where the engine's change table holds no write (seven days kept), and its write times do not follow the front's `clock:advance`; region `TABLE_STORAGE` has no physical byte columns, and an older table than 90 days no active or long-term split (#1458) | no emulator support (no compressed sizes; write times kept seven days) | S |
| Region-qualified `INFORMATION_SCHEMA.JOBS` has no `job_stages`, `object_storage_stats`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `vector_search_statistics` or `external_service_costs` (#1458) | not documented by Google (what a job without such statistics holds) | S |
| A script the emulator runs whole (no control flow, no query before its last statement, nothing it refuses together) makes no child jobs; a script's last DML statement has no child of its own, and a `SELECT` child names no destination table (#1459) | no emulator support | M |
| DML with a `FROM` of several items (#1459) | no emulator support | M |
| `CREATE SCHEMA` of an existing dataset and `CREATE TABLE … CLONE` later in a multi-statement script (#1459) | no emulator support | M |
| `CREATE [OR REPLACE] [TABLE] FUNCTION` of one that exists, and `DROP [TABLE] FUNCTION`, inside multi-statement scripts (#1459) | no emulator support | M |
| `DROP SCHEMA` where the script's other statements see the dataset; `CREATE SCHEMA` options other than the supported strings, and `DEFAULT COLLATE` (#1459) | no emulator support | M |
| `CREATE TEMP TABLE … AS SELECT` whose query cannot run alone, after one that changes data; a dry run of a script with a `TEMP` table or function, beyond what the emulator's own dry run checks (#1459) | not built yet | S |
| A failed script after a statement that wrote: the emulator does not roll back (#1459) | no emulator support | L |
| A snapshot or clone `FOR SYSTEM_TIME AS OF` a past time (a query reads one since #1460); `FOR SYSTEM_TIME AS OF` a past time of an ingestion-time partitioned table, or with a partition decorator (#1460) | not built yet | M |
| `datasets.undelete`, `UNDROP SCHEMA`, and restoring a dropped table by a copy of `table@time`: the emulator drops a table's rows and change history with it, and a dataset's tables (#1460) | no emulator support | M |
| BQML: `LINEAR_REG`, binary and multiclass `LOGISTIC_REG`, `KMEANS` and `PCA` are trained by the front, with `TRANSFORM`, ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO, ML.TRAINING_INFO, ML.CONFUSION_MATRIX, ML.ROC_CURVE, ML.CENTROIDS, ML.PRINCIPAL_COMPONENTS, ML.PRINCIPAL_COMPONENT_INFO and models.* (#1465, #1481); every other model type, remote models, ML preprocessing functions in TRANSFORM and the other ML functions are 501 with the reason | no emulator support: training Google does not document, or Google's infrastructure | L |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type (#1445) | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) (#1445) | not documented by Google | S |

## Spanner, Bigtable, Firestore, Datastore

Closed: Datastore structured aggregations and Firestore `PartitionQuery` (#1405).

| Gap | Why open | Size |
|---|---|---|
| Spanner: 30 RPCs refused, each naming why (backups, backup schedules, IAM, instance configurations, partition updates, `FetchCacheUpdate`; [spanner.md](coverage/spanner.md)); `UpdateInstance` and `UpdateDatabase` (drop protection) are served by the call front since #1461 (#1461) | no emulator support: the emulator cannot snapshot a database and has one instance configuration | L |
| Bigtable: 48 RPCs refused (authorized, logical and materialized views, memory layers, snapshots, backups, schema bundles, table IAM, `ExecuteQuery`, change streams, …; [bigtable.md](coverage/bigtable.md)) (#1461) | no emulator support | L |
| Firestore: `ExecutePipeline`; Firestore Admin: backups, restore, clone, user credentials, `ExportDocuments`, `ImportDocuments`, `BulkDeleteDocuments` (15 methods, each naming why; [firestoreadmin.md](coverage/firestoreadmin.md)); backup schedules are served since #1461 and take no backups (#1461) | no emulator support: it cannot snapshot or export a database, and is not Firestore with MongoDB compatibility | L |
| Datastore `explain_options` are 501; GQL aggregation queries are served since #1461 (#1461) | no emulator support: the emulator plans no query | S |
| Whether production reorders an array's excluded values after its indexed ones, as the emulator does (#941) | needs live GCP | S |

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
| Cloud SQL: external-primary replicas (no source representation instance is kept); `MYSQL_5_6`, `MYSQL_5_7` and SQL Server versions, and `BAK` and `TDE` files (their images are linux/amd64 only); upgrading a PostgreSQL replica on its own (streaming replication does not cross major versions); high availability, read replicas and point-in-time recovery on PostgreSQL 9.6 to 11 (no `pg_promote()` or `recovery.signal`), each 501 naming why (PostgreSQL 9.6 to 12, a MySQL replica's own upgrade, Cloud SQL's 400 for a legacy failover replica and for a PostgreSQL database's charset change are built since #1462); 24 methods 501, each naming what is missing (blue-green deployments and `connect.resolve`, server and Entra ID certificates, `demote` and external sync, `switchover`, `reencrypt`, disk shrink and others; project `Backups`, `executeSql`, `preCheckMajorVersionUpgrade`, `truncateLog` and final backups are built since #1462; [sqladmin.md](coverage/sqladmin.md)) (#1462) | not built yet | L |
| Backup and DR: workload types other than Cloud SQL instances, `encryptionConfig`, resource backup configs, management servers and trials (23 methods 501, each naming what is missing; `patch`, plan revisions, data source references and `fetchUsable` are built since #1462; [backupdr.md](coverage/backupdr.md)) (#1462) | not built yet | L |
| Memorystore for Redis: `REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` stay 501, as they are end of life with no maintained image (the Docker official `redis` image last built them in 2018, 2020 and 2022); with a customer-managed key, the RDB snapshots and AOF Valkey writes on a server's own volume are not encrypted with it, and are served only while the key can be used (#1463) | no emulator support: no maintained image; Valkey writes its own files | S |

## Functions, credentials, tooling

Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319);
Cloud Functions v2 and v1 APIs (#1377, #1428); Pub/Sub and project IAM in Terraform (#1367,
#1368).

| Gap | Why open | Size |
|---|---|---|
| The gated full-lifecycle Functions tests are not yet run: the v2 and v1 APIs' build and serve, event triggers, `gcloud functions deploy --gen2`, and the full apply of Terraform's `google_cloudfunctions2_function` and `google_cloudfunctions_function`, and `TestFunctionsSecretsBuildsAndMounts`, a build that mounts Secret Manager secrets; `repo_source` and VPC connectors, and event types no served service emits (audit logs, Firestore, Firebase), stay `UNIMPLEMENTED` with the reason (#1464) | the builder's amd64 images cannot run on arm64 nodes; no dated amd64 run yet | M |
| Terraform resources beyond the verified set (#1304): the other `google_spanner_*` resources (instance configs, backup schedules), a worker pool's `scaling_mode` (Google publishes no such field), `google_logging_folder_settings` (refused, as Google documents), `google_organization_iam_policy` (authoritative over the shared organization), `google_sql_source_representation_instance` (refused 501), `google_secret_manager_regional_secret` and `google_bigquery_data_transfer_config` (their endpoints are not set: not served), the resources whose API refuses them (Bigtable authorized views, log bucket CMEK; audit configs, tag IAM, Bigtable table IAM and Spanner IAM are stored since #1486), and the other resources of the served services | partly verified (the eight batches of #1304 and the verified rows of compatibility.md) | L |

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
