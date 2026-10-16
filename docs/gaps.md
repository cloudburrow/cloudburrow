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
| Cloud Storage | 7 | 1 | 5 | 1 | 0 |
| Pub/Sub | 2 | 0 | 0 | 2 | 0 |
| Cloud Tasks | 1 | 0 | 1 | 0 | 0 |
| Cloud Run | 6 | 2 | 3 | 0 | 1 |
| Cloud Scheduler | 2 | 0 | 1 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 6 | 0 | 3 | 2 | 1 |
| Secret Manager | 2 | 0 | 1 | 0 | 1 |
| Resource Manager | 0 | 0 | 0 | 0 | 0 |
| Vertex AI | 3 | 0 | 3 | 0 | 0 |
| BigQuery | 26 | 3 | 16 | 7 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 6 | 1 | 4 | 0 | 1 |
| Cloud SQL, Memorystore | 3 | 3 | 0 | 0 | 0 |
| Functions, credentials, tooling | 2 | 2 | 0 | 0 | 0 |
| Console | 1 | 1 | 0 | 0 | 0 |
| **Total** | **68** | **13** | **37** | **13** | **5** |

## Cloud Storage

Closed: `objects.bulkRestore` (#1279), `buckets.operations.list` filters (#1280), `channels.stop`
and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up`
instance (#1290), the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), the gRPC
`google.storage.v2` API (#1321), and its customer-supplied encryption keys, ACLs, managed-folder
IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter` (#1345). ACLs
and object IAM on the JSON API are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| gRPC `google.storage.v2`: `Bucket.ip_filter` stays `UNIMPLEMENTED` (and `ipFilter` 400 on the JSON API): enforcing it needs the caller's VPC network and the `storage.buckets.exemptFromIpFilter` permission, which are not modelled. Appendable objects, `custom_placement_config` and `SetIamPolicy`'s `update_mask` are built (#1443) | refused by design: enforcement needs what is not modelled | S |
| `managedFolders.update`: its only settable field is `rapidCacheConfig`, and an insert carrying it is 501 (#1444) | no emulator support (no cache tier) | S |
| `anywhereCaches.*`, `rapidCaches.*` (#1444) | no emulator support (no cache tier) | M |
| `buckets.relocate`, `buckets.operations.advanceRelocateBucket` (#1444) | no emulator support (one location) | M |
| `buckets.operations.cancel`: every operation is done when returned (#1444) | no emulator support | S |
| Notifications: the server with no Pub/Sub refuses a configuration (501) (#1444) | no emulator support (needs Pub/Sub) | S |
| UNVERIFIED error codes (a non-empty bucket delete, a repeat retention lock, managed-folder and folder refusals, batch limits, CORS on JSON endpoints and others; [storage.md](coverage/storage.md)) (#1445) | not documented by Google | S |

## Pub/Sub

Closed: `DetachSubscription`, `UpdateSnapshot` and Protocol Buffer schemas, served by CloudBurrow's
Pub/Sub front (#1406), and IAM on topics, subscriptions, snapshots and schemas, stored and never
enforced (#1367).

| Gap | Why open | Size |
|---|---|---|
| Any call but Get, List, Delete, Pull and StreamingPull on a detached subscription, and an `UpdateTopic` of `schema_settings` bound to a Protocol Buffer schema, answer `UNIMPLEMENTED` with the reason; the seed file still refuses `PROTOCOL_BUFFER` schemas; the front's messages for a refused `.proto` definition or message are its own (#1446) | not documented by Google (no answer to copy; Google's messages not measured) | S |
| UNVERIFIED error codes: subscription expiration, exactly-once and label refusals ([pubsub.md](coverage/pubsub.md); an invalid label's message is #982) (#1445) | not documented by Google | S |

## Cloud Tasks

Closed: `RunTask`, `BatchCreateTasks`, `BatchDeleteTasks` and the CMEK config (#1215), `oidcToken`
/ `oauthToken` on HTTP tasks and Cloud Scheduler jobs (#1243), `ListQueues` filters on any Queue
field (#1255), `stackdriver_logging_config`, with each sampled dispatch written to Cloud Logging
(#1256), and a queue-level `http_target` (#1282). Queue IAM conditions are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| App Engine targets and `app_engine_routing_override` (#1447) | no emulator support (no App Engine) | L |

## Cloud Run

Closed: IAM policy methods on Services and Jobs (#1216), and `UpdateService` with a partial
`update_mask` (#1260); container logs in Cloud Logging as `cloud_run_revision` and
`cloud_run_job` (#1298); the v1 (Knative) API, with `gcloud run services list`, `describe` and
`deploy --image` (#1293, #1297), its locations, IAM, jobs and domain mappings (#1349), job runs,
executions and tasks (#1379), and selectors, `overrides.delayExecution` and job IAM (#1420);
`Builds.SubmitBuild` (#1268); `Instances.*` (#1299) and `WorkerPools.*` (#1300), with operations
that wait for readiness and an instance's URL (#1348), and a worker pool's `update_mask` and
instance splits (#1387); secret and in-memory volumes, the service account and v1 probes (#1297),
`subPath`, Cloud SQL volumes through the Auth Proxy sidecar, and VPC access, encryption keys,
binary authorization, execution environment and session affinity stored without effect (#1346);
the service account named to its workload by the metadata server (#1388); two projects' resources
of one ID (#1400); `gcloud run deploy --source` and `gcloud builds submit` with Artifact Registry
and Cloud Build (#1347), steps of any image (#1382), a Docker registry for built images (#1383),
`secretEnv`, `script` and `allowFailure` (#1407), Cloud Build triggers and worker pools (#1422),
and the default logs bucket (#1430).

| Gap | Why open | Size |
|---|---|---|
| Cloud Storage FUSE and NFS volumes on services and jobs are refused `UNIMPLEMENTED` naming the field (#1448) | no emulator support (Knative refuses a privileged container and CSI volumes) | M |
| A service's container sees `K_SERVICE`, `K_CONFIGURATION` and `K_REVISION` as the scoped Knative names, not Cloud Run's service and revision IDs, which the API reports (#1448) | no emulator support (Knative reserves the three variables) | S |
| `CancelOperation`: a deployment in flight cannot be recalled (#1448) | no emulator support | S |
| Cloud Build: `options.dynamicSubstitutions`, `secretEnv` on a docker build, docker push or pack step, `ApproveBuild` and `approvalRequired`, and triggers with a repository or Pub/Sub source or a `filename` are refused; Artifact Registry: 27 methods `UNIMPLEMENTED` (rules, attachments, Maven, npm, Python, Apt and Yum artifacts, project and VPC-SC settings, package, version and file updates; [artifactregistry.md](coverage/artifactregistry.md)) (#1449) | not built yet | M |
| v1 (Knative) API: a domain mapping never becomes Ready, as no DNS is served and no certificate issued; a job's own `spec.template.spec.delayExecution` is 501, and a run's `overrides.delayExecution` is not read back on the execution, as v2 has no field for either (#1450) | not built yet | S |
| A stale `etag` on a job is `ABORTED`; a failed `RunJob` is `FAILED_PRECONDITION`; `CancelExecution` of an ended execution is `FAILED_PRECONDITION` (#1445) | needs live GCP | S |

## Cloud Scheduler

Closed by the sweep: Locations (#1237).

| Gap | Why open | Size |
|---|---|---|
| App Engine targets (#1447) | no emulator support (no App Engine) | L |
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
| A BigQuery sink's schema mismatch is not written to an `export_errors` table, `protoPayload` is not exported, and the sink error codes but `topic_not_found` are CloudBurrow's (#1451) | not documented by Google | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by the sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| HSM, EXTERNAL and EXTERNAL_VPC protection levels, `crypto_key_backend`, `external_protection_level_options` (#1452) | no emulator support | L |
| EKM, Autokey, AutokeyAdmin and HSM management services (#1452) | no emulator support | L |
| `key_access_justifications_policy` on `UpdateCryptoKey` (#1452) | no emulator support | S |
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
| `customer_managed_encryption` on a replica or the automatic policy (#1453) | no emulator support (needs KMS-backed CMEK) | M |
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
`ServerStreamingPredict`, `StreamingPredict` and `StreamDirectPredict` (#1285).

| Gap | Why open | Size |
|---|---|---|
| `EmbedContent` embeds text only: a non-text part, `document_ocr` and `audio_track_extraction` are refused, because the one embedding model served is text-only (#1454) | no emulator support (one text-only model) | S |
| `DirectRawPredict`, `StreamDirectRawPredict`, `StreamingRawPredict` call `method_name` on a gRPC model server; none is served (#1454) | no emulator support (no gRPC model server) | M |
| `Explain` (#1454) | no emulator support | L |

## BigQuery

Already tracked: #1418 (`CHANGES` and `APPENDS` for `enable_change_history` tables), #1426
(enforcing a table's `privacy_policy`; a query of such a table is 501), #1427 (Iceberg files for
`storage_uri`), #1434 (materialized view statements in a script with a TEMP table or function),
#1437 (`INTERVAL` arithmetic in the engine, and on the Write API's Arrow path), #1438
(`CREATE TABLE` dropping a column's `OPTIONS(description)`).

Closed: copy jobs, snapshots and clones (#1002), extracting every column type (#957),
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
| `ALTER TABLE` is 501 for a foreign key to another project's table (an instance serves one project), the table options not built, `ALTER COLUMN SET OPTIONS (data_policies, data_governance_tags)`, an `ALTER TABLE` in a script with a `TEMP` table or function or in a dry run of a script, and legacy SQL; a statement of several actions is carried out a step at a time, so a later refusal leaves the earlier steps done; a nested field's `ALTER COLUMN SET DEFAULT`, and a `WRITE_TRUNCATE_DATA` query job whose result lacks a column with a default, are 501 (#1456) | not built yet | S |
| A dry run of `CREATE VIEW` with a column list, without `OR REPLACE` or `IF NOT EXISTS`, of a view that exists is 501, as what BigQuery's dry run answers for it is not known (#1456) | not documented by Google | S |
| Table options and materialized views: `ALTER COLUMN … SET OPTIONS (rounding_mode)` is 501 where the Table OPTIONS row says so; `require_partition_filter` checks that the column is named in a `WHERE`, not that it prunes; smart tuning's rewrite of a base-table query to read a materialized view is not built (#1456); `privacy_policy` (#1426), `enable_change_history` (#1418), `storage_uri` (#1427) and materialized views in a script with a TEMP table or function (#1434) are tracked on their own | not built yet | M |
| Project configurations: `default_time_zone` does not reach `DATE`, `DATETIME`, `TIME`, `STRING` or `EXTRACT` given a column or other expression whose type the text does not show, `DATE_TRUNC` and the like, or a `TIMESTAMP` literal with no offset; the job and queue timeouts are stored, not enforced; `default_kms_key_name` reaches `tables.insert` only; `ALTER PROJECT` / `ALTER ORGANIZATION` inside a block, `IF` or loop of a script is 501; `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows (#1456) | not built yet | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name, a Parquet name with a period) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (#1457) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting, `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501 (#1457) | not documented by Google | S |
| CSV: a multi-character or non-ASCII delimiter or quote, an empty `STRING` with a `nullMarker`, a `maxBadRecords` load the emulator fails, `sourceColumnMatch` `NAME` with `skipLeadingRows` other than 1; JSON: a null field outside the schema, a `maxBadRecords` load the emulator fails (#1457) | no emulator support | M |
| Parquet loads with a NaN, and the other refused Parquet columns (#1457) | no emulator support | S |
| A load of several Cloud Storage objects when the front was given no Cloud Storage (#1457) | no emulator support | S |
| Partition decorators: `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination, a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, and through a partition a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order or a RECORD's fields changed, are 501, none documented by BigQuery (#1458) | not documented by Google | S |
| A table made by `CREATE TABLE … AS SELECT`, a temporary table, or one made before the engine had the column keeps no stream time, so its streamed rows are held from DML only while in the 20-second streaming buffer; a query naming `_PARTITION_BUFFERED` or `_PARTITION_STREAMED` reads the engine's marks; a table restored from state saved before #1277 keeps no ingestion time (#1458) | no emulator support | S |
| `PARTITIONS` has no byte counts or `last_modified_time`; region `TABLE_STORAGE` has no byte columns, `total_partitions` or `storage_last_modified_time`; `COLUMNS` has no `async_generation_status`; `OBJECT_PRIVILEGES` and `SCHEMATA_LINKS` dataset-qualified are 501 (#1458) | no emulator support (no faithful byte counts or write times) | M |
| Region-qualified `INFORMATION_SCHEMA.JOBS` has no `bi_engine_statistics`, `job_stages`, `object_storage_stats`, `session_info`, `timeline`, `query_info`, `materialized_view_statistics`, `metadata_cache_statistics`, `search_statistics`, `continuous_query_info`, `vector_search_statistics`, `external_service_costs` or `ml_statistics` (#1458) | not documented by Google (their STRUCTs) | S |
| A script the emulator runs whole (no control flow, no query before its last statement) makes no child jobs; a script's last DML statement has no child of its own, and a `SELECT` child names no destination table (#1459) | no emulator support | M |
| `MERGE` from a subquery inside a multi-statement script; DML with a `FROM` of several items (#1459) | no emulator support | M |
| `CREATE OR REPLACE TABLE`, `CREATE OR REPLACE [TEMP] TABLE`, `CREATE SCHEMA` of an existing dataset, `CREATE TABLE … LIKE`, `COPY` or `CLONE`, and `DROP TABLE` of a script's temp table, later in a multi-statement script (#1459) | no emulator support | M |
| `CREATE [OR REPLACE] [TABLE] FUNCTION` of one that exists, and `DROP [TABLE] FUNCTION`, inside multi-statement scripts (#1459) | no emulator support | M |
| `DROP SCHEMA` where the script's other statements see the dataset; `CREATE SCHEMA` options other than the supported strings, and `DEFAULT COLLATE` (#1459) | no emulator support | M |
| `CREATE TEMP TABLE … AS SELECT` whose query cannot run alone, after one that changes data; a runtime error or an `ASSERT` in a script with a `TEMP` table or function (#1459) | no emulator support | S |
| A failed script after a statement that wrote: the emulator does not roll back (#1459) | no emulator support | L |
| `FOR SYSTEM_TIME AS OF` a past time inside the time travel window, and a snapshot or clone at one, are 501: the emulator keeps no history (#1460) | no emulator support | L |
| `datasets.undelete`: the emulator keeps nothing of a deleted dataset (#1460) | no emulator support | M |
| Storage Read API: `SplitReadStream`, and `ReadRows` of a stream made before the front last started (#1460) | no emulator support | S |
| BQML: `CREATE MODEL` is refused by the engine ("Statement not supported: CreateModelStatement"), so no model can be made, used or extracted (#1465) | no emulator support | L |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type (#1445) | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) (#1445) | not documented by Google | S |

## Spanner, Bigtable, Firestore, Datastore

Closed: Datastore structured aggregations and Firestore `PartitionQuery` (#1405).

| Gap | Why open | Size |
|---|---|---|
| Spanner: 32 RPCs refused (backups, backup schedules, IAM, instance configurations, instance and partition updates, `FetchCacheUpdate`; [spanner.md](coverage/spanner.md)) (#1461) | no emulator support | L |
| Bigtable: 48 RPCs refused (authorized, logical and materialized views, memory layers, snapshots, backups, schema bundles, table IAM, `ExecuteQuery`, change streams, …; [bigtable.md](coverage/bigtable.md)) (#1461) | no emulator support | L |
| Bigtable: the admin REST API (JSON), which Terraform's `google_bigtable_app_profile` and `google_bigtable_instance_iam_*` use (#1440) | not built yet | M |
| Firestore: `ExecutePipeline`; Firestore Admin: backups, backup schedules, restore, clone, user credentials, `ExportDocuments`, `ImportDocuments`, `BulkDeleteDocuments` (20 methods; [firestoreadmin.md](coverage/firestoreadmin.md)) (#1461) | no emulator support | L |
| Datastore GQL aggregation queries (`RunAggregationQuery` with `gql_query`) and `explain_options` are 501 (#1461) | no emulator support (the emulator serves neither) | S |
| Whether production reorders an array's excluded values after its indexed ones, as the emulator does (#941) | needs live GCP | S |

## Cloud SQL, Memorystore

Closed: instance reads, databases, users and operations (#1301), instance writes, backups,
restores and clones, read replicas, point-in-time recovery and the connector path (#1330), imports
and exports, client certificates and server CA rotation, `tiers.list`, `flags.list` and
`operations.cancel` (#1380), database versions, major version upgrades, cascading and cross-region
replicas (#1381), high availability (#1408), Google's Auth Proxy binary (#1421),
`pointInTimeRestore` and Backup and DR backups through the Backup and DR Service (#1425, #1433);
the Memorystore admin API over Valkey (#1302), exports, imports and RDB persistence (#1384),
STANDARD_HA and failover (#1385), versions, upgrades and maintenance (#1386), and in-transit
encryption (#1413). IAM database authentication is excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Cloud SQL: legacy failover replicas and external-primary replicas; `MYSQL_5_7`, `POSTGRES_12` and other versions; upgrading a replica on its own; a PostgreSQL database's charset change; `BAK` and `TDE` files; 32 methods 501 (`Backups.*`, blue-green deployments, `connect.resolve`, server and Entra ID certificates, `demote`, `switchover`, `executeSql`, `reencrypt`, disk shrink, external sync and others; [sqladmin.md](coverage/sqladmin.md)) (#1462) | not built yet | L |
| Backup and DR: workload types other than Cloud SQL instances, `encryptionConfig`, `patch`, plan revisions, data source references, resource backup configs, management servers and trials (39 methods 501; [backupdr.md](coverage/backupdr.md)) (#1462) | not built yet | L |
| Memorystore for Redis: customer-managed keys are 501; `REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` stay 501, as they are end of life with no maintained image; no server CA rotation for in-transit encryption (#1463) | not built yet | M |

## Functions, credentials, tooling

Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319);
Cloud Functions v2 and v1 APIs (#1377, #1428); Pub/Sub and project IAM in Terraform (#1367,
#1368).

| Gap | Why open | Size |
|---|---|---|
| The gated full-lifecycle Functions tests are not yet run: the v2 and v1 APIs' build and serve, event triggers, `gcloud functions deploy --gen2`, and the full apply of Terraform's `google_cloudfunctions2_function` and `google_cloudfunctions_function`; `buildpack_build.runtime` and the v2 and v1 fields refused `UNIMPLEMENTED` (`repo_source`, worker pools, VPC connectors, secrets, KMS, binary authorization, other event types) (#1464) | the builder's amd64 images cannot run on arm64 nodes; no dated amd64 run yet | M |
| Terraform resources beyond the verified set (#1304): `google_firestore_backup_schedule` (backups are not served), updates of `google_spanner_instance` (the emulator does not update instances) and the other `google_spanner_*` resources, a worker pool's `scaling_mode` (Google publishes no such field), `google_cloudfunctions_function_iam_*` (untested), the Logging folder and organization resources (refused: Logging serves project parents only), `google_organization_iam_policy` (authoritative over the shared organization), `google_sql_source_representation_instance` (refused 501), `google_secret_manager_regional_secret`, `google_bigquery_data_transfer_config` and `google_cloud_run_domain_mapping` (their endpoints are not set: not served, or never Ready), `google_bigtable_app_profile` and `google_bigtable_instance_iam_*` (the bigtableadmin REST API is not served, #1440), and the other resources of the served services | partly verified (the five batches of #1304 and the verified rows of compatibility.md) | L |

## Console

Closed by the sweep: Test permissions on the Permissions tab (#1242), and the request-rate,
latency and resource charts (#1267).

| Gap | Why open | Size |
|---|---|---|
| Artifact Registry repository IAM has no Permissions tab (#1439) | not built yet | S |

## Excluded on purpose

Not gaps to close; each stays until its decision is amended.

- **Cloud Storage ACLs and object IAM** (`bucketAccessControls.*`, `objectAccessControls.*`,
  `defaultObjectAccessControls.*`, `objects.*IamPolicy`): 501 by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #504.
- **IAM Conditions and audit configs** on every policy (Tasks, Secret Manager, KMS, Storage, the
  console): refused by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #365.
- **IAM enforcement**: [ADR-0006](adr/0006-iam-policy-surface.md) stores policies, never enforces them.
- **IAM Credentials `signBlob`**: refused because a local signature makes signed URLs no Google
  service accepts ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Cloud SQL IAM database authentication** (`generateEphemeralCert` with an access token,
  `cloudsql.iam_authentication`, users of an IAM type): 501, as CloudBurrow has no identities
  ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Local AI and embeddings in per-PR CI**, and the default build's embeddings answering 501:
  maintainer decisions recorded in compatibility.md (§What CI does not run, §Local AI, #41).
