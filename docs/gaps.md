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
| Cloud Run | 14 | 11 | 1 | 0 | 2 |
| Cloud Scheduler | 2 | 0 | 1 | 0 | 1 |
| Cloud Logging | 0 | 0 | 0 | 0 | 0 |
| Cloud KMS | 6 | 0 | 3 | 2 | 1 |
| Secret Manager | 2 | 0 | 1 | 0 | 1 |
| Resource Manager | 1 | 1 | 0 | 0 | 0 |
| Vertex AI | 3 | 0 | 3 | 0 | 0 |
| BigQuery | 28 | 9 | 16 | 3 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 4 | 0 | 4 | 0 | 0 |
| Cloud SQL, Memorystore | 3 | 3 | 0 | 0 | 0 |
| Functions, credentials, tooling | 2 | 2 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **76** | **27** | **37** | **7** | **5** |

## Cloud Storage

Already tracked: none open. ACLs and object IAM are excluded (below). Closed: `objects.bulkRestore`
(#1279), `buckets.operations.list` filters (#1280), `channels.stop` and `objects.watchAll` (#1281), channel delivery on an `up` instance (#1290), and the XML API's bucket `PUT`, service `GET` and form `POST` (#1258).

| Gap | Why open | Size |
|---|---|---|
| gRPC `google.storage.v2`: not served at all, deferred until the JSON API is complete (#1321) | not built yet | L |
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

Closed: IAM policy methods on Services and Jobs (#1216), and `UpdateService` with a partial `update_mask` (#1260).

| Gap | Why open | Size |
|---|---|---|
| Refused service and job fields: service account, VPC access, volumes, encryption keys, binary authorization, execution environment, session affinity | not built yet | L |
| Logs are not served through the Cloud Run API | not built yet | M |
| `gcloud run deploy --source` is not served: it creates an Artifact Registry repository and polls Cloud Build's operations, neither served; `Builds.SubmitBuild` itself is (#1268), with its Cloud Build-only fields `UNIMPLEMENTED` | not built yet | M |
| `Instances.*` is not served | not built yet | L |
| `WorkerPools.*` is not served | not built yet | L |
| `gcloud run services list` / `describe` use the v1 (Knative-shaped) API, which is not served | not built yet | L |
| Refused service and job fields: service account, VPC access, volumes, encryption keys, binary authorization, execution environment, session affinity (#1297) | not built yet | L |
| Logs are not served through the Cloud Run API (#1298) | not built yet | M |
| `Instances.*` is not served (#1299) | not built yet | L |
| `WorkerPools.*` is not served (#1300) | not built yet | L |
| `gcloud run deploy` through the v1 (Knative) API, served since #1293 for services, revisions, routes and configurations, is not verified: its default `startupProbe` and other v1 fields are refused as not mapped, and `projects.locations.list` and the rest of v1 (domain mappings, jobs, IAM) are 501 | not built yet | M |
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

Closed: IAM policy methods, `MoveProject`, `UndeleteProject` (#1216). Closed by this sweep: v1 org policy on projects (#1296).

| Gap | Why open | Size |
|---|---|---|
| Liens, TagKeys, TagValues, TagBindings (#1295) are not served (Folders and Organizations are, #1294) | not built yet | L |

## Vertex AI

Closed: `PredictionService.EmbedContent`, over gRPC and REST `:embedContent` (#1244); `PredictionService.StreamGenerateContent` over gRPC, and an official-SDK test of `GenerateContent` over gRPC (#1257); `RawPredict` on a publisher model (#1284); `StreamRawPredict`, `DirectPredict`, `ServerStreamingPredict`, `StreamingPredict` and `StreamDirectPredict` (#1285).

| Gap | Why open | Size |
|---|---|---|
| `EmbedContent` embeds text only: a non-text part, `document_ocr` and `audio_track_extraction` are refused, because the one embedding model served is text-only (#1244) | model limit | S |
| `DirectRawPredict`, `StreamDirectRawPredict`, `StreamingRawPredict` call `method_name` on a gRPC model server; none is served (#1285) | no gRPC model server | M |
| `Explain` | no emulator support | L |

## BigQuery

Already tracked: #1201 (scripting blocks), #1037 (`EXECUTE IMMEDIATE ... INTO`), #1002 (copy jobs, snapshots, clones),
#957 (extract formats). Closed: procedures, script transactions and RANGE variables (#1217), row
access policies and table and routine IAM (#1218), infinite FLOAT64 in UNNEST (#1204), and
`ALTER TABLE` `ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` (#1227). Closed by this sweep: `routines.update` (#1238), a lone `CREATE TABLE … LIKE` or `COPY` (#1239; inside a script it stays 501), a lone `CREATE VIEW` with a column list (#1240), and a `STRUCT` query parameter with a `BYTES` field (#1241: already built by #1082 and verified by `TestBigQueryBytesParameters`; a stale `compatibility.md` row still said 501). Since then #1111, #1272 and #1274 are built: a schema update with a new column or field before the table's is refused 412 "Precondition Failed", as BigQuery refuses it, `CREATE TABLE … PARTITION BY … CLUSTER BY …` keeps its partitioning and clustering, and a dataset's `INFORMATION_SCHEMA.VIEWS` lists its views.
Already tracked: #1217 (procedures, script transactions, RANGE variables), #1218 (row access
policies, table and routine IAM), #1201 (scripting blocks), #1037 (`EXECUTE IMMEDIATE ... INTO`), #1002 (copy jobs,
snapshots, clones), #957 (extract formats), #1204 (infinite FLOAT64 in UNNEST). Closed by this sweep: `routines.update` (#1238), a lone `CREATE TABLE … LIKE` or `COPY` (#1239; inside a script it stays 501), a lone `CREATE VIEW` with a column list (#1240), and a `STRUCT` query parameter with a `BYTES` field (#1241: already built by #1082 and verified by `TestBigQueryBytesParameters`; a stale `compatibility.md` row still said 501). Since then #1111 is built: a RANGE in a STRUCT parameter is sent built as `RANGE(start, end)`, and a schema update with a new column or field before the table's is refused 412 "Precondition Failed", as BigQuery refuses it. Since then #1272 and #1274 are built: `CREATE TABLE … PARTITION BY … CLUSTER BY …` keeps its partitioning and clustering, and a dataset's `INFORMATION_SCHEMA.VIEWS` lists its views. Since then #1286 and #1275 are built: a dataset's `TABLES`, `COLUMNS`, `COLUMN_FIELD_PATHS`, `TABLE_OPTIONS`, `ROUTINES` and `PARAMETERS`, and `` `region-us`.INFORMATION_SCHEMA.JOBS ``, have BigQuery's columns.

| Gap | Why open | Size |
|---|---|---|
| `ALTER TABLE` `ALTER COLUMN SET DATA TYPE`, `SET DEFAULT` / `DROP DEFAULT`, keys and constraints are 501, and so are `RENAME TO`, `RENAME COLUMN` and `DROP COLUMN` with another action, of a nested field, of a table with row access policies or keys, and `RENAME COLUMN` of a partitioning or clustering column (`ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` are carried out since #1227, `RENAME TO`, `RENAME COLUMN` and `DROP COLUMN` since #1322) (#1334) | not built yet | M |
| A view's column list inside a multi-statement script with a `TEMP` table or function, or in a dry run, and a column option other than a `description` literal in the list, are 501 (a list in any other script, and a column's description, are carried out since #1323) (#1335) | not built yet | S |
| `NEWLINE_DELIMITED_JSON` load with `autodetect` into a new table is 501 (the emulator crashes on it) (#1324) | not built yet | M |
| CSV loads: `timeZone`, `dateFormat`, `datetimeFormat`, `timeFormat`, `timestampFormat`, and an `encoding` other than UTF-8 or ISO-8859-1 (#1325) | not built yet | M |
| `columnNameCharacterMap` `V1` / `V2` when a detected name needs renaming (#1326) | not built yet | S |
| Storage Write API `arrow_rows` (#1327) | not built yet | M |
| Partition decorators: `tabledata.list` and `tables.get` of a partition, a partition as a load, copy or query destination, and a partition of an ingestion-time partitioned table are 501 (a query's read and `tables.delete` of one are built, #1273) | not built yet | M |
| `INFORMATION_SCHEMA` `PARTITIONS` and the other dataset views but `VIEWS`, `TABLES`, `COLUMNS`, `COLUMN_FIELD_PATHS`, `TABLE_OPTIONS`, `ROUTINES` and `PARAMETERS` (built, #1274, #1286) are 501; `COLUMNS` has no `async_generation_status` (#1286) (#1328) | not built yet | M |
| Region-qualified `INFORMATION_SCHEMA.JOBS` (built, #1275) has no `bi_engine_statistics`, `job_stages`, `object_storage_stats`, `session_info`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `continuous_query_info`, `vector_search_statistics`, `external_service_costs` or `ml_statistics` (Google does not document their STRUCTs), nor child jobs; the region-qualified forms of the other views are "Table not found" (#1329) | not built yet | M |
| Partition decorators of an ingestion-time partitioned table are 501: the emulator keeps no row's ingestion time (reads, `tabledata.list`, `tables.get`, `tables.delete` and load, copy and query destinations of a column-partitioned table's partition are built, #1273) | no emulator support | M |
| `_PARTITIONTIME` and `_PARTITIONDATE` of an ingestion-time partitioned table that holds rows are 501: the emulator keeps no row's ingestion time (on any other table they fail 400 as in BigQuery, #1277) | no emulator support | M |
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

| Gap | Why open | Size |
|---|---|---|
| The Memorystore admin API (`redis.googleapis.com`) (#1302) | not built yet | L |
| The Cloud SQL Admin API beyond instance reads, databases, users and operations (served since #1301): creating or changing instances, IAM database authentication, backups, replicas, the Auth Proxy and connector path (`connectSettings`, `generateEphemeralCert`) (#1330) | not built yet | L |
| The rest of the Memorystore admin API: `ListInstances`, `GetInstance` and `GetInstanceAuthString` are served over the one Valkey server (#1302); creating, updating, upgrading, deleting, importing, exporting and failing over an instance are UNIMPLEMENTED, and the gcloud and Terraform endpoint overrides are not exported until a test drives them | not built yet | M |

## Functions, credentials, tooling

| Gap | Why open | Size |
|---|---|---|
| Functions in languages other than Go (#1303) | not built yet | L |
| Terraform resources beyond the verified set (#1304) | not built yet | L |

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
