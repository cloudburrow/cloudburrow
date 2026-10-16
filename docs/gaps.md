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
| Cloud Run | 7 | 3 | 2 | 0 | 2 |
| Cloud Scheduler | 2 | 0 | 1 | 0 | 1 |
| Cloud Logging | 0 | 0 | 0 | 0 | 0 |
| Cloud KMS | 6 | 0 | 3 | 2 | 1 |
| Secret Manager | 2 | 0 | 1 | 0 | 1 |
| Resource Manager | 0 | 0 | 0 | 0 | 0 |
| Vertex AI | 3 | 0 | 3 | 0 | 0 |
| BigQuery | 26 | 5 | 16 | 5 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 4 | 0 | 4 | 0 | 0 |
| Cloud SQL, Memorystore | 3 | 3 | 0 | 0 | 0 |
| Functions, credentials, tooling | 1 | 1 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **65** | **13** | **38** | **9** | **5** |

## Cloud Storage

Already tracked: none open. ACLs and object IAM are excluded (below). Closed: `objects.bulkRestore`
(#1279), `buckets.operations.list` filters (#1280), `channels.stop` and `objects.watchAll` (#1281), channel delivery on an `up` instance (#1290), and the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), and the gRPC `google.storage.v2` API itself (#1321).

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
| `gcloud run deploy --source` and `gcloud builds submit` are served since #1347 (Artifact Registry repositories, Cloud Build `builds.create`, `CancelBuild` and operations on the Run port, verified with gcloud on Dockerfile sources and a two-step cloudbuild.yaml); left: arbitrary step containers (only docker build, docker push and pack steps run), and no image is pushed to a registry a docker client can reach | not built yet | M |
| A worker pool's `update_mask` and instance splits across revisions are refused (served since #1300 and #1299; operations wait for the instances and an instance has a URL since #1348) | not built yet | M |
| The v1 (Knative) API's `replaceJob` and `run` (`gcloud run jobs update` and `execute`), executions and tasks are 501; a domain mapping never becomes Ready, as no DNS is served (projects.locations.list, IAM, jobs create/get/list/delete and domain mappings since #1349) | not built yet | M |
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

Closed: `TailLogEntries`, `ListMonitoredResourceDescriptors` (#1215), and log-based metrics, `MetricsServiceV2` (#1226). Closed by this sweep: the Async bucket calls, links and settings of `ConfigServiceV2` (#1292).

No gap is left open here.

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
| `ALTER TABLE` is 501 for `RENAME TO` with another action, a nested field, `RENAME TO` another dataset, a table with row access policies, `RENAME COLUMN` of a partitioning or clustering column, `RENAME COLUMN` or `DROP COLUMN` of a table with keys, and `RENAME COLUMN`, `DROP COLUMN` or `SET DATA TYPE` with an action that is not one of them (`ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` are carried out since #1227, `RENAME TO`, `RENAME COLUMN` and `DROP COLUMN` since #1322, `SET DATA TYPE` and primary and foreign keys since #1334, `ALTER COLUMN SET DEFAULT` and `DROP DEFAULT` since #1340) | not built yet | M |
| A view's column list inside a multi-statement script with a `TEMP` table or function, or in a dry run, and a column option other than a `description` literal in the list, are 501 (a list in any other script, and a column's description, are carried out since #1323) (#1335) | not built yet | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix), names the same once replaced, and a load that is not CSV, are 501 (renaming by replacing characters is built since #1326) (#1336) | not built yet | S |
| Partition decorators: through a partition, a schema change BigQuery does not document (a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order, a RECORD's fields changed), `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination and a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, none documented by BigQuery, are 501; a streamed row is modifiable by DML after the 20-second flush, not BigQuery's 30 minutes (#1370) (the rest of #1273 is built: the streaming buffer of column- and range-partitioned tables, DML refused on buffered rows, a REQUIRED column left out with `ALLOW_FIELD_RELAXATION`, #1273) | not documented by Google | S |
| A query of region-qualified `INFORMATION_SCHEMA.JOBS` with no filter the front pushes down (#1369: `job_id`, `state`, `job_type`, `project_id`, `parent_job_id`, `creation_time`, `ORDER BY creation_time LIMIT`) still inlines a row per job: `SELECT COUNT(*)` took 13 s at 5,000 jobs (54 s before #1369); a table of the job rows kept in the engine would make it a scan | not built yet (partly built, #1369) | M |
| `_PARTITIONTIME` of a row written by `tabledata.insertAll` is its partition's at once, where BigQuery reads it NULL while the row is in the streaming buffer; a table restored from state saved before #1277 keeps no ingestion time (its pseudo-columns fail 400 "Unrecognized name") (#1277) | no emulator support | S |
| `SCHEMATA_LINKS` without a region qualifier is 501 (region `OBJECT_PRIVILEGES` and `SCHEMATA_LINKS` built, #1355; `PARTITIONS`, the constraint, snapshot, routine option and index views, #1328; `SCHEMATA_OPTIONS` and `MATERIALIZED_VIEWS`, #1351); `PARTITIONS` has no byte counts or `last_modified_time`; `COLUMNS` has no `async_generation_status` (#1286) (#1355) | not built yet | M |
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
| Memorystore for Redis beyond one BASIC Valkey server per instance (#1302): STANDARD_HA (replicas, failover, read replicas), Redis versions other than `REDIS_7_2`, in-transit encryption, RDB persistence and customer-managed keys are UNIMPLEMENTED | not built yet | L |
| The rest of the Memorystore admin API: `UpgradeInstance`, `ImportInstance`, `ExportInstance`, `FailoverInstance` and `RescheduleMaintenance` are UNIMPLEMENTED; creating, updating and deleting instances, their operations, `gcloud redis` and `google_redis_instance` are served since #1302 | not built yet | M |

## Functions, credentials, tooling
Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319).


| Gap | Why open | Size |
|---|---|---|
| Terraform resources beyond the verified set (#1304): `google_firestore_backup_schedule` (backups are not served), Spanner instance updates (the emulator does not update instances, #1331), a worker pool's `scaling_mode` (#1354: refused, as Google publishes no such field for a worker pool), and the Functions, Artifact Registry and Cloud Build resources, whose endpoints the wrapper does not set | not built yet (partly verified: Pub/Sub schemas and IAM (#1367), project IAM (#1368), secret versions, Storage ACLs, ACL lists and managed folders, Scheduler HTTP targets, Cloud Tasks queue HTTP targets, Secret Manager rotation and topics, Cloud Run volumes, service accounts and worker pools, KMS key purposes and import jobs, BigQuery dataset access, dataset IAM and row access policies, partitioned and clustered tables and query jobs, `*_iam_member`, `*_iam_binding` and `*_iam_policy` on buckets, secrets, key rings, keys and queues, Firestore documents, databases, indexes and fields, Bigtable instances and tables, Spanner instances and databases, Logging bucket configs and linked datasets, `google_project_service`) | L |

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
