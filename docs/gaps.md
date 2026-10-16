# Remaining gaps

Every place [compatibility.md](compatibility.md) or [docs/coverage](coverage/README.md) says
something is not implemented (501, `UNIMPLEMENTED`, Unimplemented, Not served, Refused), was not
measured, or rests on an UNVERIFIED choice, swept on 2026-10-15. Left out: what an open issue
already tracks (listed per service), and what an ADR or a recorded maintainer decision excludes
on purpose (the last section). The per-operation detail stays in compatibility.md; this page
is the to-do view of it. Gaps filed by the sweep carry their issue number.

**Why it is open:**

- *not built yet*: nothing stops it but work;
- *no emulator support*: the upstream emulator refuses it, or there is nothing local for it to act on;
- *not documented by Google*: CloudBurrow had to choose a code, a message or a rule (`UNVERIFIED`);
- *needs live GCP*: only an observation of Google would settle it, and nothing here calls Google.

**Size:** S is a day or less, M a few days, L a new subsystem.

## Counts

| Service | Gaps | Not built yet | No emulator support | Not documented by Google | Needs live GCP |
|---|---|---|---|---|---|
| Cloud Storage | 7 | 1 | 5 | 1 | 0 |
| Pub/Sub | 3 | 0 | 2 | 1 | 0 |
| Cloud Tasks | 1 | 0 | 1 | 0 | 0 |
| Cloud Run | 19 | 12 | 5 | 0 | 2 |
| Cloud Scheduler | 2 | 0 | 1 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 6 | 0 | 3 | 2 | 1 |
| Secret Manager | 2 | 0 | 1 | 0 | 1 |
| Resource Manager | 0 | 0 | 0 | 0 | 0 |
| Vertex AI | 3 | 0 | 3 | 0 | 0 |
| BigQuery | 45 | 21 | 16 | 8 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 4 | 0 | 4 | 0 | 0 |
| Cloud SQL, Memorystore | 9 | 9 | 0 | 0 | 0 |
| Functions, credentials, tooling | 2 | 2 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **104** | **45** | **41** | **13** | **5** |

## Cloud Storage

Already tracked: none open. ACLs and object IAM are excluded (below). Closed: `objects.bulkRestore`
(#1279), `buckets.operations.list` filters (#1280), `channels.stop` and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up` instance (#1290), and the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), and the gRPC `google.storage.v2` API itself (#1321).

| Gap | Why open | Size |
|---|---|---|
| gRPC `google.storage.v2` (#1321): served on the JSON API's port; `UNIMPLEMENTED` over gRPC are appendable objects (zonal buckets only, which are not modelled; the store has no unfinalized object) (customer-supplied encryption keys, on both APIs, the `acl` fields, managed-folder IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter`, on both APIs, are carried out since #1345) (#1345) | not built yet | M |
| `managedFolders.update`: its only settable field is `rapidCacheConfig`, and an insert carrying it is 501 | no emulator support (no cache tier) | S |
| `anywhereCaches.*`, `rapidCaches.*` | no emulator support (no cache tier) | M |
| `buckets.relocate`, `buckets.operations.advanceRelocateBucket` | no emulator support (one location) | M |
| `buckets.operations.cancel`: every operation is done when returned | no emulator support | S |
| Notifications: the server with no Pub/Sub refuses a configuration (501) | no emulator support (needs Pub/Sub) | S |
| 14 UNVERIFIED error codes (a non-empty bucket delete, a repeat retention lock, managed-folder and folder refusals, batch limits, CORS on JSON endpoints and others; [storage.md](coverage/storage.md)) | not documented by Google | S |

## Pub/Sub

Already tracked: #982 (Google's message for an invalid label is not measured).

| Gap | Why open | Size |
|---|---|---|
| `DetachSubscription`, `UpdateSnapshot` are refused `UNIMPLEMENTED` by Google's emulator | no emulator support | M |
| Protocol Buffer schemas: `ValidateSchema` of `PROTOCOL_BUFFER` is `UNIMPLEMENTED` | no emulator support | M |
| 4 UNVERIFIED error codes: subscription expiration, exactly-once and label refusals ([pubsub.md](coverage/pubsub.md)) | not documented by Google | S |

## Cloud Tasks

Queue IAM conditions are excluded (below). Closed: `RunTask`, `BatchCreateTasks`,
`BatchDeleteTasks` and the CMEK config (#1215), `oidcToken` / `oauthToken` on HTTP tasks and
Cloud Scheduler jobs (#1243), `ListQueues` filters on any Queue field (#1255), and
`stackdriver_logging_config`, with each sampled dispatch written to Cloud Logging (#1256), and a
queue-level `http_target` (#1282).

| Gap | Why open | Size |
|---|---|---|
| App Engine targets and `app_engine_routing_override` | no emulator support (no App Engine) | L |

## Cloud Run

Closed: IAM policy methods on Services and Jobs (#1216), and `UpdateService` with a partial `update_mask` (#1260); container logs in Cloud Logging as `cloud_run_revision` and `cloud_run_job` (#1298); the v1 (Knative) API, with `gcloud run services list`, `describe` and `deploy --image` (#1293, #1297), and its locations, IAM, jobs and domain mappings (#1349); `Builds.SubmitBuild` (#1268); `Instances.*` (#1299) and `WorkerPools.*` (#1300), with operations that wait for readiness and an instance's URL (#1348).

| Gap | Why open | Size |
|---|---|---|
| Refused service and job fields (#1346): Cloud Storage FUSE volumes (Knative refuses a privileged container and CSI volumes) and NFS volumes; the service account is stored but tokens name the instance's account (secret and in-memory volumes, the service account and v1 probes mapped in #1297; `subPath` mapped, Cloud SQL volumes served by the Cloud SQL Auth Proxy sidecar, and VPC access, encryption keys, binary authorization, execution environment and session affinity stored and returned without effect, in #1346, with v1's `run.googleapis.com/cloudsql-instances` annotation as the same volume) | no emulator support (Knative refuses FUSE and NFS volumes) | M |
| `gcloud run deploy --source` and `gcloud builds submit` are served since #1347 (Artifact Registry repositories, Cloud Build `builds.create`, `CancelBuild` and operations on the Run port, verified with gcloud on Dockerfile sources and a two-step cloudbuild.yaml); since #1382 steps of any image run as containers on the host's Docker (socket, `cloudbuild` network, shared `/workspace`), and `ListBuilds` `filter` and paging and `RetryBuild` are served; left: a step's `secretEnv`, `script` and `allowFailure`, and no image is pushed to a registry a docker client can reach | not built yet | S |
| A worker pool's `update_mask` and instance splits across revisions are refused (served since #1300 and #1299; operations wait for the instances and an instance has a URL since #1348) | not built yet | M |
| Refused service and job fields: VPC access, Cloud SQL, Cloud Storage FUSE and NFS volumes, `subPath`, encryption keys, binary authorization, execution environment, session affinity (the service account named to its workload by the metadata server in #1388; secret and in-memory volumes, the service account and v1 probes mapped in #1297) (#1346) | not built yet | L |
| `gcloud run deploy --source` is not served: it creates an Artifact Registry repository and polls Cloud Build's operations, neither served; `Builds.SubmitBuild` itself is (#1268), with its Cloud Build-only fields `UNIMPLEMENTED` (#1347) | not built yet | M |
| Worker pool and standalone instance operations complete when written, not when their instances are ready; an instance gets no URL (`ingress` refused) (served since #1300 and #1299; a worker pool's `update_mask` and instance splits across revisions served in #1387) (#1348) | not built yet | M |
| `gcloud run deploy` through the v1 (Knative) API, served since #1293 for services, revisions, routes and configurations, is not verified: its default `startupProbe` and other v1 fields are refused as not mapped, and `projects.locations.list` and the rest of v1 (domain mappings, jobs, IAM) are 501 (#1349) | not built yet | M |
Closed: IAM policy methods on Services and Jobs (#1216), and `UpdateService` with a partial `update_mask` (#1260); container logs in Cloud Logging as `cloud_run_revision` and `cloud_run_job` (#1298); the v1 (Knative) API, with `gcloud run services list`, `describe` and `deploy --image` (#1293, #1297), and its locations, IAM, jobs and domain mappings (#1349); `Builds.SubmitBuild` (#1268).
| Refused service and job fields (#1346): Cloud SQL volumes (needs a Cloud SQL Auth Proxy sidecar and the 3307 connector published to pods), Cloud Storage FUSE volumes (Knative refuses a privileged container and CSI volumes) and NFS volumes (the service account named to its workload by the metadata server in #1388; secret and in-memory volumes, the service account and v1 probes mapped in #1297; `subPath` mapped, and VPC access, encryption keys, binary authorization, execution environment and session affinity stored and returned without effect, in #1346) | not built yet | M |
| Refused service and job fields (#1346): Cloud Storage FUSE volumes (Knative refuses a privileged container and CSI volumes) and NFS volumes (the service account named to its workload by the metadata server in #1388; secret and in-memory volumes, the service account and v1 probes mapped in #1297; `subPath` mapped, Cloud SQL volumes served by the Cloud SQL Auth Proxy sidecar, and VPC access, encryption keys, binary authorization, execution environment and session affinity stored and returned without effect, in #1346; v1's `run.googleapis.com/cloudsql-instances` annotation is still refused) | no emulator support (Knative refuses FUSE and NFS volumes) | M |
| Refused service and job fields (#1346): Cloud Storage FUSE volumes (Knative refuses a privileged container and CSI volumes) and NFS volumes (the service account named to its workload by the metadata server in #1388; secret and in-memory volumes, the service account and v1 probes mapped in #1297; `subPath` mapped, Cloud SQL volumes served by the Cloud SQL Auth Proxy sidecar, and VPC access, encryption keys, binary authorization, execution environment and session affinity stored and returned without effect, in #1346, with v1's `run.googleapis.com/cloudsql-instances` annotation as the same volume) | no emulator support (Knative refuses FUSE and NFS volumes) | M |
| `gcloud run deploy --source` is not served: it creates an Artifact Registry repository and polls Cloud Build's operations, neither served; `Builds.SubmitBuild` itself is (#1268), with its Cloud Build-only fields `UNIMPLEMENTED` | not built yet | M |
| Refused service and job fields: VPC access, Cloud SQL, Cloud Storage FUSE and NFS volumes, `subPath`, encryption keys, binary authorization, execution environment, session affinity (the service account named to its workload by the metadata server in #1388; secret and in-memory volumes, the service account and v1 probes mapped in #1297) | not built yet | L |
| `Instances.*` is not served (#1299) | not built yet | L |
| `WorkerPools.*` is not served (#1300) | not built yet | L |
| A v1 (Knative) domain mapping never becomes Ready, as no DNS is served; v1 `overrides.delayExecution` and set-based `labelSelector`s (`gcloud run jobs executions tasks list --succeeded`) are 501 (projects.locations.list, IAM, jobs create/get/list/delete and domain mappings since #1349; `replaceJob`, `run`, executions and tasks since #1379) | not built yet | S |
| A service's container sees `K_SERVICE`, `K_CONFIGURATION` and `K_REVISION` as the scoped Knative names (`<id>-<scope>`, `<id>-<scope>-00001`), not Cloud Run's service and revision IDs, which the API reports: Knative reserves the three variables (two projects' services of one ID are two since #1400) | no emulator support (Knative reserves `K_SERVICE`, `K_CONFIGURATION` and `K_REVISION`) | S |
| `CancelOperation`: a deployment in flight cannot be recalled | no emulator support | S |
| A stale `etag` on a job is `ABORTED`; a failed `RunJob` is `FAILED_PRECONDITION` | needs live GCP | S |
| `CancelExecution`'s result | needs live GCP | S |

## Cloud Scheduler

Closed by this sweep: Locations (#1237).

| Gap | Why open | Size |
|---|---|---|
| App Engine targets | no emulator support (no App Engine) | L |
| `ResumeJob` on an enabled job and the default `retry_config` are inferred from the Terraform provider | needs live GCP | S |

## Cloud Logging

Closed: `TailLogEntries`, `ListMonitoredResourceDescriptors` (#1215), and log-based metrics, `MetricsServiceV2` (#1226). Closed by this sweep: the Async bucket calls, links and settings of `ConfigServiceV2` (#1292), and `CopyLogEntries` (#1394). Closed: sink delivery to Cloud Storage, Pub/Sub, BigQuery and another project, and `writer_identity` (#1404). Closed: `CopyLogEntries` from folder, organization and billing-account buckets, with their Operations JSON bindings (#1411).

| Gap | Why open | Size |
|---|---|---|
| A BigQuery sink's schema mismatch is not written to an `export_errors` table, `protoPayload` is not exported, and the sink error codes but `topic_not_found` are CloudBurrow's (#1404) | not documented by Google | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by this sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| HSM, EXTERNAL and EXTERNAL_VPC protection levels, `crypto_key_backend`, `external_protection_level_options` | no emulator support | L |
| EKM, Autokey, AutokeyAdmin and HSM management services | no emulator support | L |
| `key_access_justifications_policy` on `UpdateCryptoKey` | no emulator support | S |
| REST: `alt` other than JSON, and `fields`/`$fields`, are `UNIMPLEMENTED` | not documented by Google | S |
| 72 UNVERIFIED error codes ([kms.md](coverage/kms.md)) | not documented by Google | S |
| Project-number parents are refused; whether Google accepts them, and whether an empty key ring can be deleted | needs live GCP | S |

## Secret Manager

IAM conditions are excluded (below). Closed: `RotateSecret`, `EnableManagedRotation` and
`Secret.rotation` (#1215), `Secret.expire_time` / `ttl` (#1225), and `ListSecrets` /
`ListSecretVersions` filters (#1228), `Secret.topics` event notifications to Pub/Sub (#1259), and
`Secret.rotation` schedules and `version_destroy_ttl` delayed destruction (#1278).

| Gap | Why open | Size |
|---|---|---|
| `customer_managed_encryption` on a replica or the automatic policy | no emulator support (needs KMS-backed CMEK) | M |
| Replication immutability is documented, not measured; 1 UNVERIFIED code (ephemeral restart) | needs live GCP | S |

## Resource Manager

Closed: IAM policy methods, `MoveProject`, `UndeleteProject` (#1216). Closed by this sweep: v1 org policy on projects (#1296), and regional tag bindings and effective tags through the `{location}-cloudresourcemanager` endpoints (#1350). Closed: project, folder and organization IAM, stored and never enforced (#1368).

| Gap | Why open | Size |
|---|---|---|
No gap is left open here.

## Vertex AI

Closed: `PredictionService.EmbedContent`, over gRPC and REST `:embedContent` (#1244); `PredictionService.StreamGenerateContent` over gRPC, and an official-SDK test of `GenerateContent` over gRPC (#1257); `RawPredict` on a publisher model (#1284); `StreamRawPredict`, `DirectPredict`, `ServerStreamingPredict`, `StreamingPredict` and `StreamDirectPredict` (#1285).

| Gap | Why open | Size |
|---|---|---|
| `EmbedContent` embeds text only: a non-text part, `document_ocr` and `audio_track_extraction` are refused, because the one embedding model served is text-only (#1244) | no emulator support (one text-only model) | S |
| `DirectRawPredict`, `StreamDirectRawPredict`, `StreamingRawPredict` call `method_name` on a gRPC model server; none is served (#1285) | no emulator support (no gRPC model server) | M |
| `Explain` | no emulator support | L |

## BigQuery

Already tracked: #957 (extract formats). Closed: copy jobs, snapshots and clones (#1002), a JSON load's field outside the schema (#1339), `EXECUTE IMMEDIATE` as a child job, `INTO`'s errors and `@@row_count` (#1037), scripting control flow, handlers and failed statements' child jobs (#1201), procedures, script transactions and RANGE variables (#1217), row access policies and table and routine IAM (#1218), infinite FLOAT64 in UNNEST (#1204), `ALTER TABLE` `ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` (#1227), `routines.update` (#1238), a lone `CREATE TABLE … LIKE` or `COPY` (#1239), a lone `CREATE VIEW` with a column list (#1240), a `STRUCT` parameter with a `BYTES` field (#1241, #1082), a schema update with a new column before the table's and a RANGE in a STRUCT parameter (#1111), `CREATE TABLE … PARTITION BY … CLUSTER BY …` (#1272), `INFORMATION_SCHEMA` `VIEWS` (#1274), the dataset views (#1286, #1328), region `JOBS` and the region views (#1275, #1329), time travel `AS OF CURRENT_TIMESTAMP()` (#1276), `RENAME TO`, `RENAME COLUMN`, `DROP COLUMN` (#1322), `SET DATA TYPE` and keys (#1334), a view's column list in a script (#1323), a JSON load with `autodetect` (#1324), CSV `timeZone`, SQL-style formats and encodings (#1325), and `columnNameCharacterMap` renaming (#1326); the Storage Write API's `arrow_rows` (#1327), C-style CSV formats and formats with `autodetect` (#1338), column defaults (#1340), `INFORMATION_SCHEMA` `SCHEMATA_OPTIONS` and `MATERIALIZED_VIEWS` (#1351) and region `OBJECT_PRIVILEGES` and `SCHEMATA_LINKS` (#1355), a script's child jobs (#1353), ingestion time and partition decorators (#1277, #1273), and filtered `INFORMATION_SCHEMA.JOBS` queries (#1369).
Already tracked: #1201 (scripting blocks), #1037 (`EXECUTE IMMEDIATE ... INTO`), #1002 (copy jobs, snapshots, clones), #957 (extracting a model, as BQML is not supported), #1339 (a JSON load's field outside the schema). Closed: procedures, script transactions and RANGE variables (#1217), row access policies and table and routine IAM (#1218), infinite FLOAT64 in UNNEST (#1204), `ALTER TABLE` `ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` (#1227), `routines.update` (#1238), a lone `CREATE TABLE … LIKE` or `COPY` (#1239), a lone `CREATE VIEW` with a column list (#1240), a `STRUCT` parameter with a `BYTES` field (#1241, #1082), a schema update with a new column before the table's and a RANGE in a STRUCT parameter (#1111), `CREATE TABLE … PARTITION BY … CLUSTER BY …` (#1272), `INFORMATION_SCHEMA` `VIEWS` (#1274), the dataset views (#1286, #1328), region `JOBS` and the region views (#1275, #1329), time travel `AS OF CURRENT_TIMESTAMP()` (#1276), `RENAME TO`, `RENAME COLUMN`, `DROP COLUMN` (#1322), `SET DATA TYPE` and keys (#1334), a view's column list in a script (#1323), a JSON load with `autodetect` (#1324), CSV `timeZone`, SQL-style formats and encodings (#1325), and `columnNameCharacterMap` renaming (#1326).

| Gap | Why open | Size |
|---|---|---|
| `ALTER TABLE` is 501 for `SET DATA TYPE` of a partitioning or clustering column, from `INT64` to a parameterized `NUMERIC`, to an `ARRAY` or of a `STRUCT`'s REPEATED field, a foreign key to another project's table, and in a dry run (`ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` are carried out since #1227, `RENAME TO`, `RENAME COLUMN` and `DROP COLUMN` since #1322, `SET DATA TYPE` and primary and foreign keys since #1334, `ALTER COLUMN SET DEFAULT` and `DROP DEFAULT` since #1340; actions of different ways in one statement, row access policies, keys and `STRUCT` coercions since #1389, refusing what the DDL reference refuses); a statement of several ways is carried out a run at a time, so a later refusal leaves the earlier runs done | not built yet | S |
| A view's column list inside a multi-statement script with a `TEMP` table or function, or in a dry run, and a column option other than a `description` literal in the list, are 501 (a list in any other script, and a column's description, are carried out since #1323) (#1335) | not built yet | S |
| A view's column list in a script given a dry run or by `EXECUTE IMMEDIATE`, a dry run of a plain `CREATE VIEW` with a list of a view that exists, and a column option other than a `description` literal in the list, are 501 (a list in a script with a `TEMP` table or function, and a dry run of a lone one, are carried out since #1335) (#1335) | not built yet | S |
| A view's column list inside a multi-statement script, and a column's `OPTIONS` in the list (a lone `CREATE VIEW v (c, d) AS …` is carried out, #1240) (#1323) | not built yet | S |
| `NEWLINE_DELIMITED_JSON` load with `autodetect` into a new table is 501 (the emulator crashes on it) (#1324) | not built yet | M |
| CSV loads: `timeZone`, `dateFormat`, `datetimeFormat`, `timeFormat`, `timestampFormat`, and an `encoding` other than UTF-8 or ISO-8859-1 (#1325) | not built yet | M |
| `ALTER TABLE` `ALTER COLUMN SET DEFAULT` / `DROP DEFAULT` are 501: the engine applies a column default only from the `CREATE TABLE` that made its table, never from the table's `defaultValueExpression` (measured, #1340); the other `ALTER TABLE` forms are the row above (#1389) (`ADD COLUMN, `SET OPTIONS` and `ALTER COLUMN` are carried out since #1227, `RENAME TO`, `RENAME COLUMN` and `DROP COLUMN` since #1322, `SET DATA TYPE` and primary and foreign keys since #1334) (#1340) | not built yet | M |
| What #1390 leaves of table `OPTIONS` and materialized views: `default_rounding_mode` and a column's `rounding_mode` round top-level parameterized NUMERIC and BIGNUMERIC columns only (#1401: not a STRUCT's field or an ARRAY's element, and `ALTER TABLE ADD COLUMN … OPTIONS (rounding_mode)` is 501); `privacy_policy` is stored and a query of the table refused 501 (not enforced); `enable_change_history` is stored, `CHANGES` and `APPENDS` not served; `storage_uri` is stored, no Iceberg files written; `require_partition_filter` checks that the column is named in a `WHERE`, not that it prunes; a materialized view is not refreshed on a change to a base table (only by `CALL BQ.REFRESH_MATERIALIZED_VIEW` and its interval) and smart tuning is not built; options and materialized views in a multi-statement script, and options in a script with a TEMP table or function | not built yet | M |
| `columnNameCharacterMap` `V1` / `V2` when a detected name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix), names the same once replaced, and a load that is not CSV, are 501 (renaming by replacing characters is built since #1326) (#1336) | not built yet | S |
| Partition decorators: through a partition, a schema change BigQuery does not document (a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order, a RECORD's fields changed), `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination and a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, none documented by BigQuery, are 501; a streamed row is modifiable by DML after the 20-second flush, not BigQuery's 30 minutes (#1370) (the rest of #1273 is built: the streaming buffer of column- and range-partitioned tables, DML refused on buffered rows, a REQUIRED column left out with `ALLOW_FIELD_RELAXATION`, #1273) | not documented by Google | S |
| A query of region-qualified `INFORMATION_SCHEMA.JOBS` with no filter the front pushes down (#1369: `job_id`, `state`, `job_type`, `project_id`, `parent_job_id`, `creation_time`, `ORDER BY creation_time LIMIT`) still inlines a row per job: `SELECT COUNT(*)` took 13 s at 5,000 jobs (54 s before #1369); a table of the job rows kept in the engine would make it a scan | not built yet (partly built, #1369) | M |
| CSV loads: a C-style (`%m/%d/%Y`) date or time format, and `timeZone` or a format with `autodetect` and no schema (SQL-style formats, `timeZone` and the UTF-16 and UTF-32 encodings are built, #1325) (#1338) | not built yet | S |
| Storage Write API `arrow_rows` (#1327) | not built yet | M |
| Locations (#1371, #1375): `ALTER ORGANIZATION`, every configuration but `default_location` (`default_time_zone`, the `region-REGION.` ones) and `ALTER PROJECT` inside a script are 501; `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows (Google does not say whether BigQuery does); `SCHEMATA` in a script is read as the script starts, so a dataset the script makes is 404 there; its `ddl` leaves out `DEFAULT COLLATE` | not built yet | S |
| Partition decorators: `$__NULL__` or `$__UNPARTITIONED__` as a destination, `WRITE_TRUNCATE_DATA` into a partition, a schema update through one partition, and `tabledata.insertAll` into a partition are 501 (reads, `tabledata.list`, `tables.get`, `tables.delete` and load, copy and query destinations are built, of an ingestion-time partitioned table too, #1273, #1277) | not built yet | M |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized", not how a name replacing characters cannot fix is (renaming by replacing characters is built since #1326; the map refused for a format other than CSV and Parquet, and a Parquet name refused under `STRICT`, since #1336) | not documented by Google | S |
| `columnNameCharacterMap` `V1` / `V2` of a Parquet file whose name is still refused once its characters are replaced (a period, which BigQuery keeps, "except periods in Parquet file column names"; a `V1` leading digit) and names the same once replaced are 501: BigQuery does not document how it names them (renaming the file's columns at every depth, and a flexible name under `STRICT`, are built since #1373) | not documented by Google | S |
| Partition decorators: through a partition, a schema change BigQuery does not document (a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order, a RECORD's fields changed), `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination and a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, none documented by BigQuery, are 501 (the rest of #1273 is built: the streaming buffer of column- and range-partitioned tables, DML refused on buffered rows, a REQUIRED column left out with `ALLOW_FIELD_RELAXATION`, #1273; and since #1370 DML refuses a row streamed in the last 30 minutes, on any table) | not documented by Google | S |
| `INFORMATION_SCHEMA` `PARTITIONS` and the other dataset views but `VIEWS`, `TABLES`, `COLUMNS`, `COLUMN_FIELD_PATHS`, `TABLE_OPTIONS`, `ROUTINES` and `PARAMETERS` (built, #1274, #1286) are 501; `COLUMNS` has no `async_generation_status` (#1286) (#1328) | not built yet | M |
| Region-qualified `INFORMATION_SCHEMA.JOBS` (built, #1275) has no `bi_engine_statistics`, `job_stages`, `object_storage_stats`, `session_info`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `continuous_query_info`, `vector_search_statistics`, `external_service_costs` or `ml_statistics` (Google does not document their STRUCTs), nor child jobs; the region-qualified forms of the other views are "Table not found" (#1329) | not built yet | M |
| `INFORMATION_SCHEMA` `MATERIALIZED_VIEWS`, `OBJECT_PRIVILEGES`, `SCHEMATA_OPTIONS` and `SCHEMATA_LINKS` are 501 (`PARTITIONS`, the constraint, snapshot, routine option and index views built, #1328); `PARTITIONS` has no byte counts or `last_modified_time`; `COLUMNS` has no `async_generation_status` (#1286) | not built yet | M |
| Region-qualified `INFORMATION_SCHEMA.JOBS` (built, #1275) has no `bi_engine_statistics`, `job_stages`, `object_storage_stats`, `session_info`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `continuous_query_info`, `vector_search_statistics`, `external_service_costs` or `ml_statistics` (Google does not document their STRUCTs, and the front knows none of their contents, #1329), nor child jobs; region `TABLE_STORAGE` has no byte counts (#1329) | not built yet | M |
| `_PARTITIONTIME` of a row written by `tabledata.insertAll` is its partition's at once, where BigQuery reads it NULL while the row is in the streaming buffer; a table restored from state saved before #1277 keeps no ingestion time (its pseudo-columns fail 400 "Unrecognized name") (#1277) | no emulator support | S |
| `SCHEMATA_LINKS` without a region qualifier is 501 (region `OBJECT_PRIVILEGES` and `SCHEMATA_LINKS` built, #1355; `PARTITIONS`, the constraint, snapshot, routine option and index views, #1328; `SCHEMATA_OPTIONS` and `MATERIALIZED_VIEWS`, #1351); `PARTITIONS` has no byte counts or `last_modified_time`; `COLUMNS` has no `async_generation_status` (#1286) (#1355) | not built yet | M |
| Partition decorators: `tabledata.list` and `tables.get` of a partition, a partition as a load, copy or query destination, and a partition of an ingestion-time partitioned table are 501 (a query's read and `tables.delete` of one are built) (#1273) | not built yet | M |
| `PARTITIONS` has no byte counts or `last_modified_time`; `COLUMNS` has no `async_generation_status` (#1286) (`SCHEMATA_LINKS` without a region qualifier built, #1392; region `OBJECT_PRIVILEGES` and `SCHEMATA_LINKS`, #1355; `PARTITIONS`, the constraint, snapshot, routine option and index views, #1328; `SCHEMATA_OPTIONS` and `MATERIALIZED_VIEWS`, #1351) (#1355) | not built yet | M |
| Region-qualified `INFORMATION_SCHEMA.JOBS` (built, #1275) has no `bi_engine_statistics`, `job_stages`, `object_storage_stats`, `session_info`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `continuous_query_info`, `vector_search_statistics`, `external_service_costs` or `ml_statistics` (Google does not document their STRUCTs, and the front knows none of their contents, #1329); region `TABLE_STORAGE` has no byte counts | not documented by Google | S |
| A script the emulator runs whole (no control flow, no query before its last statement) makes no child jobs, as the emulator runs it as one job; a script's last DML statement has no child of its own, and a `SELECT` child names no destination table (child jobs of a script the front carries out, `jobs.list` `parentJobId` and `INFORMATION_SCHEMA.JOBS` `parent_job_id`, are built, #1353) | no emulator support | M |
| Materialized views (`CREATE` / `DROP MATERIALIZED VIEW`) | no emulator support | L |
| `datasets.undelete`: the emulator keeps nothing of a deleted dataset | no emulator support | M |
| `FOR SYSTEM_TIME AS OF` a past time inside the time travel window is 501: the emulator keeps no history (`AS OF CURRENT_TIMESTAMP()` reads the table, and a time before the window fails 400 as in BigQuery, #1276) | no emulator support | L |
| A failed script after a statement that wrote: the emulator does not roll back | no emulator support | L |
| `MERGE` from a subquery inside a multi-statement script; DML with a `FROM` of several items | no emulator support | M |
| `CREATE OR REPLACE TABLE`, `CREATE OR REPLACE [TEMP] TABLE`, `CREATE SCHEMA` of an existing dataset, and `DROP TABLE` of a script's temp table, later in a multi-statement script | no emulator support | M |
| `CREATE [OR REPLACE] [TABLE] FUNCTION` of one that exists, and `DROP [TABLE] FUNCTION`, inside multi-statement scripts | no emulator support | M |
| `DROP SCHEMA` where the script's other statements see the dataset | no emulator support | M |
| `CREATE SCHEMA` options other than the supported strings, and `DEFAULT COLLATE` | no emulator support | M |
| `CREATE TEMP TABLE … AS SELECT` whose query cannot run alone, after one that does | no emulator support | S |
| CSV: a multi-character or non-ASCII delimiter or quote, an empty `STRING` with a `nullMarker`, a `maxBadRecords` load the emulator fails | no emulator support | M |
| Parquet loads with a NaN, and the other refused Parquet columns | no emulator support | S |
| Storage Read API: `SplitReadStream`, and `ReadRows` of a stream made before the front last started | no emulator support | S |
| A load of several Cloud Storage objects when the front was given no Cloud Storage | no emulator support | S |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type | not documented by Google | S |
| CSV header detection when every column reads as `STRING` | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) | not documented by Google | S |

## Spanner, Bigtable, Firestore, Datastore

| Gap | Why open | Size |
|---|---|---|
| Spanner: 32 RPCs refused (backups, backup schedules, IAM, instance configurations, instance and partition updates, `FetchCacheUpdate`; [spanner.md](coverage/spanner.md)) | no emulator support | L |
| Bigtable: 34 RPCs refused (authorized views, snapshots, backups, IAM, `ExecuteQuery`, …; [bigtable.md](coverage/bigtable.md)) | no emulator support | L |
| Firestore `PartitionQuery` (`GetPartitionedQueries`) | no emulator support | M |
| Datastore `RunAggregationQuery` (count, sum, avg) | no emulator support | M |

## Cloud SQL, Memorystore

Closed: instance reads, databases, users and operations (#1301), instance writes, backups, replicas and the connector path (#1330), and the Memorystore admin API over Valkey (#1302).

| Gap | Why open | Size |
|---|---|---|
| The Cloud SQL Admin API beyond instance reads and writes (a server per created instance, POSTGRES_17 and MYSQL_8_4), databases, users, operations (#1301), the connector path (`connectSettings`, `generateEphemeralCert` and TLS on 3307, served for the Go connector), backups, restores and clones over real dumps, read replicas (streaming and GTID replication, promote, stop and start), and point-in-time recovery of created instances (archived WAL, the binary log; clone to a time, getLatestRecoveryTime; `transactionLogRetentionDays` and retained backups applied, with a retention clock), all #1330: IAM database authentication (no identities, ADR-0004), `pointInTimeRestore` (a Backup and DR datasource), failover replicas and HA (501: no synchronous standby in a second zone), cascading and cross-region replicas, other database versions, and a pinned Auth Proxy test (#1330) | not built yet | L |
| The Memorystore admin API (`redis.googleapis.com`) (#1302) | not built yet | L |
| The Cloud SQL Admin API beyond instance reads and writes (a server per created instance, POSTGRES_17 and MYSQL_8_4), databases, users, operations (#1301), the connector path (`connectSettings`, `generateEphemeralCert` and TLS on 3307, served for the Go connector), backups, restores and clones over real dumps, read replicas (streaming and GTID replication, promote, stop and start), and point-in-time recovery of created instances (archived WAL, the binary log; clone to a time, getLatestRecoveryTime; `transactionLogRetentionDays` and retained backups applied, with a retention clock), all #1330, and imports and exports (SQL and CSV, to and from the instance's Cloud Storage), client certificates and server CA rotation (`sslCerts`, `resetSslConfig`, `addServerCa`, `rotateServerCa`, `listServerCas`), `tiers.list`, `flags.list` and `operations.cancel` (#1380): parallel imports and exports, BAK/TDE files, IAM database authentication (no identities, ADR-0004), `pointInTimeRestore` (a Backup and DR datasource), failover replicas and HA (501: no synchronous standby in a second zone), a major version upgrade of an instance with replicas, and a pinned Auth Proxy test (#1330, #1381) | not built yet | L |
| The rest of the Memorystore admin API: `ListInstances`, `GetInstance` and `GetInstanceAuthString` are served over the one Valkey server (#1302); creating, updating, upgrading, deleting, importing, exporting and failing over an instance are UNIMPLEMENTED, and the gcloud and Terraform endpoint overrides are not exported until a test drives them | not built yet | M |
| Memorystore for Redis beyond one BASIC Valkey server per instance (#1302): STANDARD_HA (replicas, failover, read replicas), Redis versions other than `REDIS_7_2`, in-transit encryption, RDB persistence and customer-managed keys are UNIMPLEMENTED | not built yet | L |
| The rest of the Memorystore admin API: `UpgradeInstance`, `ImportInstance`, `ExportInstance`, `FailoverInstance` and `RescheduleMaintenance` are UNIMPLEMENTED; creating, updating and deleting instances, their operations, `gcloud redis` and `google_redis_instance` are served since #1302 | not built yet | M |
| Memorystore for Redis beyond one BASIC Valkey server per instance (#1302): STANDARD_HA (replicas, failover, read replicas), Redis versions other than `REDIS_6_X`, `REDIS_7_0` and `REDIS_7_2` (#1386), in-transit encryption and customer-managed keys are UNIMPLEMENTED | not built yet | L |
| Memorystore for Redis beyond BASIC and STANDARD_HA Valkey servers (#1302, #1385): Redis versions other than `REDIS_6_X`, `REDIS_7_0` and `REDIS_7_2` (#1386), in-transit encryption and customer-managed keys are UNIMPLEMENTED | not built yet | L |
| The Cloud SQL Admin API beyond instance reads, databases, users and operations (served since #1301): creating or changing instances, IAM database authentication, backups, replicas, the Auth Proxy and connector path (`connectSettings`, `generateEphemeralCert`) (#1330) | not built yet | L |

## Functions, credentials, tooling
Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319).


| Gap | Why open | Size |
|---|---|---|
| Terraform resources beyond the verified set (#1304): `google_firestore_backup_schedule` (backups are not served), Spanner instance updates (the emulator does not update instances, #1331), a worker pool's `scaling_mode` (#1354: refused, as Google publishes no such field for a worker pool), and the Functions, Artifact Registry and Cloud Build resources, whose endpoints the wrapper does not set | not built yet (partly verified: Pub/Sub schemas and IAM (#1367), project IAM (#1368), secret versions, Storage ACLs, ACL lists and managed folders, Scheduler HTTP targets, Cloud Tasks queue HTTP targets, Secret Manager rotation and topics, Cloud Run volumes, service accounts and worker pools, KMS key purposes and import jobs, BigQuery dataset access, dataset IAM and row access policies, partitioned and clustered tables and query jobs, `*_iam_member`, `*_iam_binding` and `*_iam_policy` on buckets, secrets, key rings, keys and queues, Firestore documents, databases, indexes and fields, Bigtable instances and tables, Spanner instances and databases, Logging bucket configs and linked datasets, `google_project_service`) | L |
| A dated run of the Node.js, Python, Java, Ruby, PHP and .NET Functions tests (#1319) | fixtures and gated tests for all six exist; none has run, since the builder's amd64 images cannot run on arm64 nodes | S |
| Terraform resources beyond the verified set (#1304): `google_pubsub_*_iam_*` (Pub/Sub serves no IAM methods, #1367), `google_project_iam_*` (ADR-0006 keeps project IAM UNIMPLEMENTED; a decision is needed, #1368), `google_firestore_backup_schedule` (backups are not served), Spanner instance updates (the emulator does not update instances, #1331), a worker pool's `scaling_mode` (#1354: refused, as Google publishes no such field for a worker pool), `google_cloudfunctions2_function` (served since #1377; the full apply is gated on amd64 nodes and not yet run) and v1 functions (refused by the wrapper: the v1 API is not served), and `google_cloudbuild_trigger` and `_worker_pool` (refused 501: Cloud Build serves builds only) | partly verified (Artifact Registry repositories, Cloud Run services with a Cloud SQL volume, Pub/Sub schemas, secret versions, Storage ACLs, ACL lists and managed folders, Scheduler HTTP targets, Cloud Tasks queue HTTP targets, Secret Manager rotation and topics, Cloud Run volumes, service accounts and worker pools, KMS key purposes and import jobs, BigQuery dataset access, dataset IAM and row access policies, partitioned and clustered tables and query jobs, `*_iam_member`, `*_iam_binding` and `*_iam_policy` on buckets, secrets, key rings, keys and queues, Firestore documents, databases, indexes and fields, Bigtable instances and tables, Spanner instances and databases, Logging bucket configs and linked datasets, `google_project_service`) | L |
| Functions in languages other than Go (#1303) | not built yet | L |

## Console

Closed by this sweep: Test permissions on the Permissions tab (#1242), and the request-rate, latency and resource charts (#1267).

No gap is left open here.

## Excluded on purpose

Not gaps to close; each stays until its decision is amended.

- **Cloud Storage ACLs and object IAM** (`bucketAccessControls.*`, `objectAccessControls.*`,
  `defaultObjectAccessControls.*`, `objects.*IamPolicy`): 501 by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #504.
- **IAM Conditions and audit configs** on every policy (Tasks, Secret Manager, KMS, Storage, the
  console): refused by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #365.
- **IAM enforcement**: [ADR-0006](adr/0006-iam-policy-surface.md) stores policies, never enforces them.
- **IAM Credentials `signBlob`**: refused because a local signature makes signed URLs no Google
  service accepts ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Local AI and embeddings in per-PR CI**, and the default build's embeddings answering 501:
  maintainer decisions recorded in compatibility.md (§What CI does not run, §Local AI, #41).
