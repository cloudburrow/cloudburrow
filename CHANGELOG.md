# Changelog

All notable changes to CloudBurrow are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); before 1.0.0, a minor
version may change behaviour.

The release workflow publishes a tag's section of this file as the release notes, and refuses
to publish a tag that has no section here. Before tagging `vX.Y.Z`, move the entries under
**Unreleased** into a new `## [X.Y.Z] - YYYY-MM-DD` section and add its link at the bottom.

## [Unreleased]

## [0.2.0] - 2026-11-18

The second release. The generated coverage now tracks thirty API surfaces, up from nine in
v0.1.0, and counts **1,133 of 1,227 methods Verified** through official clients, up from 97 of
191 ([docs/coverage](docs/coverage/README.md)). New since v0.1.0: Cloud Run's v1 (Knative) API,
jobs and worker pools; Cloud Build, Artifact Registry, Secure Source Manager and Developer Connect;
Cloud Functions v1 and v2; the Cloud SQL Admin API and Backup and DR; the Memorystore for Redis,
Redis Cluster and Valkey admin APIs; IAM custom roles, predefined roles and service accounts;
Service Usage; Firestore's Admin API and Firestore with MongoDB compatibility; Bigtable's instance
admin API and SQL; the BigQuery Storage and Connection APIs, and BigQuery ML; and Resource Manager
folders, organizations, tags and liens. CloudBurrow now builds its own Cloud Storage server and
its own BigQuery emulator, and BigQuery serves every Resource Manager project. Local AI embeds
text, images, audio, video and PDFs with EmbeddingGemma 2, OCR included, and serves Vertex AI's
`PredictionService` over gRPC. IAM gains roles, service accounts and Conditions; policies are
stored and still not enforced, with five named exceptions under **Changed**. The console has a
Cloud Shell-style terminal and a screen for every service. Releases are smoke-tested before they
become latest, and macOS signing and notarization are ready once the maintainer adds the Apple
secrets. Only a row of [docs/compatibility.md](docs/compatibility.md) marked Verified is a support
claim; [docs/gaps.md](docs/gaps.md) lists what is still open.

### Added

#### Cloud Storage

- **CloudBurrow's own Cloud Storage server**, built to the API, is now the only Cloud Storage
  backend (#485, #519). It serves buckets and objects over JSON, every upload type including
  resumable and XML multipart, ranged reads, every precondition, listing with prefixes, offsets,
  globs and pages, compose, copy, rewrite and move, the batch endpoint, versioning, soft delete and
  restore, retention policies, locks and holds, lifecycle rules, CORS, HMAC keys, notifications to
  Pub/Sub, the XML API, and signed URLs that fail closed (#489–#509, #1258). It runs as a locally
  built image in one Deployment, with reset, seed, state snapshots, persistence by `--mode`, events,
  metrics and faults, and console pages (#510–#514, #518).
- The gRPC API, `google.storage.v2`, on the same port, sharing one store with JSON (#1321, #1345,
  #1443), including appendable objects and customer-supplied encryption keys.
- Hierarchical namespace folders, managed folders and their updates, bucket long-running
  operations, relocation, Anywhere Cache and rapid caches, `objects.bulkRestore`, object contexts,
  and Storage Intelligence configurations (#828, #1203, #1279, #1280, #1444, #1505, #1519, #1532,
  #1700).
- Bucket and object ACLs, and bucket and managed-folder IAM policies, are stored and returned, never
  enforced (#504, #847, #1233).
- `softDeletePolicy.hardDeletePause`, held to Google's rules (#1702, #1715).
- Signed URLs verify against the instance's ADC key, `storage.signingCerts` and the certificates of
  IAM service accounts (#577, #1787).
- Verified through the official Python and Node.js clients, `gcloud storage`, `gsutil` and
  Terraform, and compared against Google's `storage-testbench` by a nightly oracle (#497,
  #515–#517, #589).

#### Pub/Sub

- **Subscription expiration** (#873, #891): a front in the Pub/Sub pod deletes a subscription idle
  for its `expirationPolicy.ttl` (31 days by default), counts calls and open streaming pulls as
  activity, applies expiration updates that Google's emulator refuses, and works over gRPC and
  REST alike (#908). Exactly-once delivery is verified, and is refused with a push endpoint, as on
  Google (#880).
- Pub/Sub in `state save` and `state load`: schemas with their revisions, topics, subscriptions
  with every setting, and snapshots, but not messages (#880). Seed files take schemas, a topic's
  `schemaSettings`, and a subscription's expiration, retention and exactly-once settings (#890,
  #899).
- Topic and subscription label updates checked against Google's rules, `DetachSubscription`,
  `UpdateSnapshot` and Protocol Buffer schema validation (#949, #962, #1406, #1446).
- IAM policies on topics, subscriptions, snapshots and schemas, stored and not enforced (#1367).
- The front recreates every topic, subscription, schema and snapshot after the emulator restarts,
  so no client sees an empty emulator. Messages that were not yet acknowledged are still lost
  (#1608).
- Pub/Sub calls are recorded in `/metrics`, the request log and traces, as the in-process
  services' calls are (#1266).

#### Cloud Tasks

- `UpdateQueue` over gRPC and REST, `RunTask`, `BatchCreateTasks`, `BatchDeleteTasks`, and CMEK
  configuration, stored only (#692, #1215).
- Rate limits are enforced: `max_dispatches_per_second` and `max_concurrent_dispatches`, with
  queues dispatched concurrently and `dispatch_deadline` honoured per attempt (#579, #716).
  `max_burst_size` is CloudBurrow's own choice, because Google does not document it.
- `oidcToken` and `oauthToken` on HTTP tasks, minted locally and signed with the instance's key
  (#1243).
- Queue-level `http_target`, `stackdriver_logging_config`, `ListQueues` filters, and App Engine
  targets delivered to the server named by `--appengine-url` (#1222, #1255, #1256, #1282, #1447,
  #1506).

#### Cloud Run

- **Jobs, executions and tasks** run as Kubernetes batch Jobs (#582, #1223), and **worker pools and
  instances** (#1299, #1300, #1348, #1387).
- **Scaling**: service-level `scaling` (automatic and manual, including manual zero instances),
  revision `cpuUtilization` and `concurrencyUtilization`, and worker-pool automatic scaling over
  REST, which CloudBurrow drives on the pods' CPU utilization; the console edits service scaling
  (#1788, #1789, #1790, #1800, #1802).
- Traffic splits, tags and pinned revisions, mapped to Knative (#1224).
- The service account, Secret Manager and in-memory volumes, Cloud SQL volumes through the Auth
  Proxy, NFS volumes, Cloud Storage FUSE volumes from the instance's own storage, the `h2c` port
  name and `K_SERVICE`, `K_CONFIGURATION` and `K_REVISION` (#1297, #1346, #1388, #1448, #1474,
  #1475, #1476).
- IAM policies on services, jobs and worker pools, stored and not enforced (#1216).
- The v1 (Knative-shaped) Admin API that `gcloud run` uses, including jobs, executions, domain
  mappings, instances and worker pools (#1293, #1349, #1379, #1420, #1533, #1546).
- `gcloud run deploy --source` and `Builds.SubmitBuild`, and a REST surface for Cloud Run v2 that
  gcloud and Terraform reach (#591, #1268, #1347).
- Container logs in Cloud Logging, with `logUri`, and operations with `ListOperations` filters
  and `WaitOperation` (#1229, #1263, #1298, #1333).
- Two projects can each have a service, job or worker pool with the same ID (#1399, #1400).

#### Cloud Build, Artifact Registry, Secure Source Manager and Developer Connect

- **Cloud Build**: builds with docker build, push and pack steps; steps of any image when
  `--cloudbuild-host-steps` is set; `secretEnv`, `script`, `allowFailure` and `allowExitCodes`;
  `RetryBuild`, approvals and dynamic substitutions; manual, webhook and Pub/Sub triggers; private
  worker pools, stored; logs written to the default logs bucket (#1382, #1407, #1422, #1430,
  #1449).
- **Artifact Registry**: a Docker registry that clients push to and pull from, with no
  authentication; Maven, npm, Python, Apt and Yum repositories; packages, versions, tags, files,
  rules, remote repositories over another repository, project configuration and Locations; and
  repository IAM, stored and not enforced (#1383, #1439, #1531, #1539, #1546, #1578).
- **Secure Source Manager** instances and repositories, git over HTTP, and push triggers into Cloud
  Build (#1521, #1527); **Developer Connect** connections and git repository links (#1546).

#### Cloud Functions

- The Cloud Functions v2 API (2nd gen, HTTP functions) and the v1 API (1st gen), with `gcloud
  functions` and Terraform (#1377, #1428).
- v2 `eventTrigger` for Pub/Sub and Cloud Storage events, delivered locally (#1414); secrets, KMS
  keys, worker pools and binary authorization on functions (#1464).
- Functions in Node.js, Python, Java, Ruby, PHP and .NET, built with Google's buildpacks (#1303,
  #1319).

#### BigQuery

- **CloudBurrow builds its own BigQuery emulator** (#1061): goccy/bigquery-emulator v0.8.1, built
  from pinned sources with patches to its SQL engine (`third_party/bigquery-emulator`) and embedded
  in the CLI. A validating front in the emulator's pod holds IDs, schemas, rows and statements to
  BigQuery's rules, and answers 501 for what the emulator cannot run (#861, #901, #902).
- **Every Resource Manager project is a BigQuery project** (#1764, #1767, #1781): datasets, tables
  and jobs per project, queries, DML, loads, copies, clones and snapshots across projects, and
  `projects.list` from Resource Manager. A name without a project is resolved in the job's project
  (#1784).
- DDL: `ALTER TABLE` (add, rename and drop columns, `SET DATA TYPE`, defaults, keys, options),
  views with column lists, materialized views, `CREATE TABLE … LIKE`, `COPY` and `CLONE`, snapshot
  tables, partitioning and clustering, column defaults, `und:ci` collation, and table, column and
  schema `OPTIONS` (#1227, #1239, #1272, #1322, #1334, #1340, #1390, #1415, #1438, #1456, #1508).
- Scripting: `IF`, `LOOP`, `WHILE`, `REPEAT`, `FOR`, `CASE`, `BEGIN … EXCEPTION`, `RAISE`,
  `ASSERT`, `EXECUTE IMMEDIATE`, stored procedures, multi-statement transactions, and child jobs
  (#1037, #1201, #1217, #1253, #1353, #1459).
- `INFORMATION_SCHEMA`: dataset views and region-qualified views, including `JOBS`,
  `JOBS_BY_FOLDER` and `JOBS_BY_ORGANIZATION` (#1274, #1275, #1286, #1328, #1329, #1351, #1355,
  #1392).
- Time travel with `FOR SYSTEM_TIME AS OF`, dataset and table undelete, and change history through
  `APPENDS` and `CHANGES` (#1276, #1418, #1460).
- Loads: Avro, ORC and nested or repeated Parquet; CSV and JSON options, bad-record reporting and
  load statistics; autodetect, `columnNameCharacterMap`, hive partitioning, manifest file sets and
  GeoJSON; and loads from the instance's Cloud Storage (#919, #944, #952, #960, #966, #988, #1004,
  #1324, #1326, #1339, #1457, #1547, #1713).
- Extract jobs to the instance's Cloud Storage as CSV or newline-delimited JSON, with GZIP (#939,
  #957), and copy jobs that keep partitioning and clustering (#987, #1759).
- The Storage Read API (Arrow and Avro) and the Storage Write API (proto and Arrow rows, every
  stream type, missing values, RANGE and INTERVAL columns) (#1095, #1098, #1102, #1327, #1391,
  #1424).
- Routines (`routines.*`), table functions, models (`models.*`), `datasets.patch`, `update` and
  `undelete`, `projects.getServiceAccount` and row access policies (#1047, #1177, #1205, #1218,
  #1238).
- Table and routine IAM policies, stored, with Terraform's `google_bigquery_table_iam_*`
  (#1231, #1236). The per-project checks for minted tokens are under **Changed**.
- `privacy_policy` (aggregation threshold, join restriction and differential privacy), BigQuery
  tables for Apache Iceberg, and object tables over Cloud Storage with signed URLs (#1426, #1427,
  #1455, #1482, #1783, #1786).
- **BigQuery ML**: `CREATE MODEL` with linear and logistic regression, k-means, PCA, matrix
  factorization, boosted trees, random forests, DNNs, autoencoders, and ARIMA_PLUS with custom
  holidays, trained locally; imported ONNX, TensorFlow Lite and TensorFlow SavedModel models; and
  remote models over the local Vertex AI endpoint (`ML.GENERATE_TEXT`, `ML.GENERATE_EMBEDDING`)
  (#1465, #1481, #1518, #1548, #1562, #1600, #1711, #1760, #1765, #1775, #1776). compatibility.md
  lists the model types and functions served.
- **AI functions** on the local Vertex AI endpoint, never Google: `AI.GENERATE` with
  `output_schema`, `AI.GENERATE_BOOL`, `_INT`, `_DOUBLE` and `_TABLE` through constrained decoding,
  and `AI.GENERATE_EMBEDDING`, `AI.EMBED`, `AI.SIMILARITY`, `AI.CLASSIFY`, `AI.IF` and `AI.SCORE`
  (#1771, #1772, #1779). Remote models honour `max_output_tokens` and the sampling options
  (#1797).
- Remote models over Cloud AI services: `REMOTE_SERVICE_TYPE = 'CLOUD_AI_VISION_V1'` with
  `ML.ANNOTATE_IMAGE` text detection over an object table, read locally with Tesseract; other
  service types are 501 (#1773).
- Image functions over object tables: `ML.DECODE_IMAGE`, `ML.RESIZE_IMAGE`,
  `ML.CONVERT_COLOR_SPACE`, `ML.CONVERT_IMAGE_TYPE`, and `ML.PREDICT` of an imported TensorFlow
  Lite or TensorFlow image model over them (#1798).
- The BigQuery Connection API over REST and gRPC, with connection IAM. A deleted service account
  in a connection's policy reads as `deleted:` and is dropped on purge, as in other services'
  policies (#1755, #1770, #1795).
- Query parameters of every top-level type, `STRUCT` and `RANGE` parameters, DML row counts and
  statement types, and the `bq` CLI's `mk`, `insert`, `query` and `rm` (#696, #1008, #1082, #1108,
  #1111).

#### Firestore and Datastore

- The Firestore Admin API: databases, indexes, field overrides, backup schedules, backups, restore
  and clone, exports and imports, and user credentials (#1331, #1500).
- **Firestore with MongoDB compatibility** (#1703, #1738): CloudBurrow's own MongoDB wire-protocol
  server on `--port-firestore-mongodb`, authenticated with the Admin API's user credentials or
  with an access token the instance minted. It serves CRUD, aggregation, indexes, transactions,
  `explain` and text search over the database's Firestore documents.
- Firestore pipelines, `PartitionQuery`, and Datastore `RunAggregationQuery` and `explain_options`
  (#1405, #1500, #1613).
- Firestore keeps its documents across a pod restart and an emulator crash in persistent mode
  (#1615).
- Every Firestore and Datastore method coverage had marked Unknown is now verified through the
  official SDK (#1197).

#### Bigtable and Spanner

- Bigtable's instance admin API over gRPC and REST: instances, clusters, app profiles, logical
  views, memory layers, and IAM, stored (#1331, #1440, #1499).
- Bigtable authorized views, snapshots, backups, schema bundles, `UndeleteTable` and change streams
  (#1499, #1511, #1523).
- **Bigtable SQL** through `ExecuteQuery`, including the documented functions and geography
  functions (#1499, #1511, #1704, #1720, #1728, #1735).
- Spanner: the 55 methods coverage had marked Unknown, backups and backup schedules, instance
  configurations and partitions, database roles, and IAM, stored (#1199, #1486, #1498).
- Terraform `google_bigtable_*`, `google_spanner_*` and `google_firestore_*` resources (#1331,
  #1440, #1461).

#### Cloud SQL and Backup and DR

- **The Cloud SQL Admin API** (`sqladmin` v1 and v1beta4) on `--port-sqladmin`, over real
  PostgreSQL and MySQL servers (#1301): instances, databases and users; backups; read replicas,
  cascading and cross-region; high availability and failover; point-in-time recovery; import and
  export; SSL and server certificates; and the connector path that the Cloud SQL connectors and
  Auth Proxy use (#1330, #1380, #1408, #1425).
- PostgreSQL 9.6 to 18 and MySQL 8.0 and 8.4, major version upgrades, clones to
  binary log coordinates, parallel imports and exports, external primaries and their syncs, DR
  replicas and switchover, blue-green deployments, CMEK and disk shrink (#1381, #1421, #1501,
  #1524, #1535, #1538, #1570, #1614, #1769).
- **The Backup and DR Service API** for Cloud SQL (`--services backupdr`): backup vaults, plans,
  associations, data sources and restores, with gcloud and Terraform (#1433, #1462, #1536).
- Terraform's `google_sql_*` resources (#1301, #1304, #1330).

#### Memorystore

- **The Memorystore for Redis admin API** (`google.cloud.redis.v1`, gRPC and REST) over real Valkey
  servers, one Deployment per instance (#1302): create, update, delete, export and import, RDB
  persistence, the Standard HA tier with read replicas and failover, upgrades, maintenance, CMEK
  and in-transit encryption (#1384, #1385, #1386, #1412, #1413, #1463, #1768).
- **Memorystore for Redis Cluster and Memorystore for Valkey**: clusters and instances, token and
  IAM auth, ACL policies, backups, migration, cross-instance replication, Valkey 7.2 and 9.0, and
  CMEK (#1519, #1549, #1557, #1563, #1578, #1614).
- `gcloud redis`, `gcloud memorystore`, Terraform and console screens for all three.

#### Secret Manager

- User-managed replica locations, `expire_time` and `ttl`, rotation schedules, `topics`
  notifications to Pub/Sub, `version_destroy_ttl`, and list filters (#857, #1225, #1228, #1259,
  #1278).
- Regional secrets and customer-managed encryption keys, including CMEK-protected versions mounted
  into Cloud Run (#1453, #1489, #1519).

#### Cloud KMS

- Asymmetric signing and decryption, MAC, raw symmetric and key encapsulation (ML-KEM) keys,
  `GenerateRandomBytes`, key deletion and retired resources, and import jobs (#1212).
  `EC_SIGN_SECP256K1_SHA256` signing (#1719).
- Automatic rotation (`rotation_period`, `next_rotation_time`), editable in the console (#816).
- EXTERNAL and EXTERNAL_VPC keys, EKM connections, Autokey, single-tenant HSM management, key
  access justifications and trusted key wrapping, without any real HSM or external key manager
  (#1452, #1469, #1506, #1521). `Locations` is served too (#1291).
- `gcloud kms` and `cloudburrow terraform` for key rings, keys, versions and IAM policies, which are
  stored and not enforced (#425–#431).

#### Cloud Scheduler

- `UpdateJob`, a REST surface, `Locations`, OIDC and OAuth tokens minted locally, App Engine
  targets, and Terraform's `google_cloud_scheduler_job` (#591, #795, #1237, #1243, #1304, #1447).
- State save, seeding, faults, tracing, the request log and metrics, as the other services have
  (#600).

#### Cloud Logging

- Log-based metrics, sinks, exclusions, log buckets and views, links, settings and CMEK settings
  (#1226, #1264, #1292, #1311).
- Sinks deliver to Cloud Storage, Pub/Sub, BigQuery and another project, and intercepting sinks
  (#1404, #1451, #1521).
- Folder, organization and billing-account parents, Log Analytics linked datasets that BigQuery
  can query, `CopyLogEntries`, `TailLogEntries`, saved queries, log scopes and log view IAM
  (#1215, #1394, #1395, #1403, #1410, #1423, #1435, #1519).
- `gcloud logging` and Terraform's `google_logging_*` resources (#1246, #1332, #1403).

#### Resource Manager, IAM and Service Usage

- **Folders and the local organization**, `MoveProject`, tag keys, values, bindings and holds, and
  liens, over v1, v2 and v3, with Terraform (#1294, #1295, #1350).
- Project, folder and organization IAM policies, stored and not enforced, with IAM Conditions
  stored and validated as Google documents them (#1368, #1742, #1745), and org policies on
  projects, folders and the organization (#1296, #1534).
- `UndeleteProject`, v1 `projects.update`, and `POST /admin/purge` (#1216, #1265, #1537).
- **IAM**: custom roles, Google's 2,423 predefined roles, `queryGrantableRoles` and
  `queryTestablePermissions`; service accounts and their keys; `LintPolicy`,
  `QueryAuditableServices` and the deprecated `SignBlob` and `SignJwt`; and the certificate URLs
  of service accounts, with gcloud, Terraform and console pages (#1743, #1746, #1747, #1750,
  #1751).
- IAM Credentials: `generateIdToken`, `signJwt` and `signBlob`, beside `generateAccessToken`
  (#1747).
- **Service Usage** (`serviceusage.googleapis.com`) on `--port-serviceusage`, for `gcloud services`
  and Terraform's `google_project_service`. Disabling an API is recorded and not enforced (#1352,
  #1530).

#### Vertex AI and local AI

- **Embeddings**: a community ONNX conversion of EmbeddingGemma for text (#41), and one of
  EmbeddingGemma 2 for text, images, audio, video and PDFs, including `document_ocr` on scanned
  pages with Tesseract OCR (#1454, #1651, #1721–#1724, #1729, #1731–#1734, #1736, #1739). Both are
  labelled as community conversions, not Google artifacts.
- **`PredictionService` over gRPC**, for `cloud.google.com/go/aiplatform`: `Predict`, `RawPredict`,
  `GenerateContent`, `StreamGenerateContent`, `EmbedContent`, `Explain` with Sampled Shapley, and
  the streaming and direct predict methods, raw ones relayed to a container's `grpcPorts` (#1193,
  #1244, #1257, #1284, #1285, #1454, #1475, #1477).
- Local generation honours `temperature`, `topP`, `topK`, `seed`, `maxOutputTokens` and
  `stopSequences` on a newer LiteRT-LM, and `responseSchema` through constrained decoding; the
  console playground offers them (#1779, #1797).
- The local-AI runtime image is published with each release, `ghcr.io/cloudburrow/litert-lm:<tag>`,
  and a release CLI uses it by digest, so local generation no longer needs a checkout (#602).
- Console: model download, load and unload, an embedding view, and custom prediction requests from
  the playground (#869, #1305, #1306, #1396).

#### Console

- **A terminal in the top bar, like Cloud Shell** (#781, #834): a shell in a pod in the instance's
  cluster, never on this machine, from the pinned Cloud SDK image, with the instance's gcloud
  configuration and no credential. It has tabs, keeps its sessions across a reload, shows progress
  during the first image pull, and refuses any other origin (#824, #826).
- A screen for every service, and every feature reachable from it, checked by three parity audits
  (#782, #1552, #1591): edits, create options and IAM Permissions tabs across services (#784–#802,
  #852–#854, #1242); BigQuery, Firestore, Datastore, Bigtable, Spanner, Cloud SQL and Memorystore
  screens (#698, #868, #993–#999, #1207, #1343, #1358); fault injection, state save and load,
  reset, seed, and a Connect and About panel (#800–#802).
- Tested in headless Chrome in CI, at several viewport sizes and against a WCAG 2.1 AA audit (#594,
  #700, #1604).

#### CLI, tools and SDKs

- `--port-base` runs instances side by side, and `--cloudsql-connector` moves the Cloud SQL
  connector listener (#584, #1364).
- `cloudburrow seed` and `events`, `env --format kubernetes`, and endpoints injected into Cloud Run
  revisions; the metadata server and CLI-hosted APIs resolve from pods (#575, #576, #586, #707).
- `cloudburrow prefetch` and `up --offline` for air-gapped use (#604); see "Offline and air-gapped
  use" in [docs/install.md](docs/install.md#offline-and-air-gapped-use).
- Clusters are stamped with the CLI, node-image and Knative versions, and `up` reconciles them
  (#601). State written by v0.1.0, in the state directory and in a `state save` archive, loads in
  this release, Cloud Storage included; a nightly test upgrades a real v0.1.0 instance (#1605,
  #1606, #1714).
- `gcloud-setup` writes the overrides for every API the instance serves, and `cloudburrow
  terraform` sets the matching custom endpoints. Terraform is verified for the resources
  compatibility.md lists, and OpenTofu for the modules its tests apply (#682, #719, #1304, #1578).
- `--emulator-heap-mib` and `--bigquery-heap-mib` bound the emulators' memory (#1603, #1618).
- The Python suite covers the opt-in emulators, Cloud KMS, BigQuery and impersonation, and a new
  Node.js suite covers Storage, Pub/Sub, Cloud Tasks, Secret Manager and Firestore (#589, #592).

#### Releases

- A release is built only from a commit whose `ci-green` check passed, is published as a
  prerelease, and is marked latest and pushed to the Homebrew tap only after it smoke-installs on
  each platform's native runner (#596, #603, #680, #689).
  [docs/install.md](docs/install.md#a-release-failed-its-smoke-test) says how to withdraw a
  release.
- The macOS binaries are signed with a Developer ID certificate and notarized once the maintainer
  adds the Apple secrets. Until then, releases are unsigned and the release notes say so (#605).
  [docs/install.md](docs/install.md#gatekeeper) explains how to check a binary.
- Release notes come from this changelog, and [SECURITY.md](SECURITY.md) says how to report a
  vulnerability (#603).

### Changed

#### BigQuery

- **BigQuery serves every ACTIVE Resource Manager project, and only those** (#1764). v0.1.0 served
  one project. A project that Resource Manager does not have is now 404 "project … is not found",
  a project in `DELETE_REQUESTED` is 403, and `projects.list` lists Resource Manager's projects.
  Create a project in Resource Manager, or seed one, before using it in BigQuery.
- **A name without a project now resolves in the job's project** (#1784): `dataset.table`, views,
  functions, procedures and models, and the default dataset's `projectId`. Before, the first
  project to create an object of that name won.
- **A request with an access token the instance minted is checked against that account's roles**
  (#1767, #1774): `bigquery.jobs.create` on the job's project, and data, dataset, table, routine,
  model and job permissions, with inherited roles, IAM Conditions and authorized views. Refusals are
  403 `accessDenied`. Requests with no bearer token, or with the ADC fixture's token, are served
  as before.
- **Remote models need their connection's permissions** (#1765): `CREATE MODEL … REMOTE WITH
  CONNECTION` and every query of a remote model need `bigquery.connections.use` on the connection
  (403 otherwise), and need the connection's service account to hold `roles/aiplatform.user` on the
  model's project (400 otherwise). `WITH CONNECTION DEFAULT` uses the project's default
  connection, and creates it when the caller holds `bigquery.connections.create`. Remote models
  are new in this release (#1760); this changes how they answered before #1765.
- A `TIMESTAMP` literal with no time zone, and casts between dates, datetimes and timestamps, now
  use UTC as BigQuery does, not the emulator's America/Los_Angeles (#1188, #1189).
- `tables.get` and every job report times in milliseconds, not seconds (#971, #1310). A retried
  `jobs.insert` of an existing job ID is 409 duplicate, not 400 (#1622). An unknown table or
  dataset in a query is 404 `notFound`, not `jobInternalError` (#1582), and a failed query gives
  the same error through `jobs.query` and `jobs.insert` (#1581).
- Query results go to a hidden `_cloudburrow_query_results` dataset and expire after 24 hours or
  past 200 results, as BigQuery's anonymous datasets do. `jobs.getQueryResults` of an expired
  result is 404 (#1017, #1059, #1086).
- The emulator listens only on loopback in its pod, so pods reach it through the front's checks
  (#1114). Enabling BigQuery needs a CLI with the embedded emulator and storage server; a CLI built
  with a plain `go build` refuses `up` with BigQuery, and `doctor` reports it (#902, #1061). The CLI
  is about 86 MB larger.

#### IAM, projects and credentials

- **IAM policies are still not enforced, with five exceptions**, each recorded in
  [ADR-0006](docs/adr/0006-iam-policy-surface.md): BigQuery's per-project checks of minted tokens
  (above, #1767, #1774); BigQuery remote models' connection permissions (above, #1765);
  Memorystore for Redis Cluster and Valkey IAM auth (#1614); Firestore with MongoDB
  compatibility's token sign-in and command permissions (#1738); and IAM Credentials
  (`generateAccessToken`, `generateIdToken`, `signBlob`, `signJwt`, and the IAM API's deprecated
  `SignBlob` and `SignJwt`) on service accounts the IAM API created (#1747, #1751). All but the
  remote-model check accept only an access token this instance minted, for a principal holding the
  documented role. Calls as the instance's own account are checked no more than in v0.1.0.
- **One project number everywhere** (#1211, #1544): the metadata server's `numeric-project-id`,
  zone and machine type now carry the project's own number rather than `000000000000`, and
  Resource Manager v3 names a project `projects/{number}`, as Google does. A project recreated
  under a purged ID gets a new number.
- **Deleting a project keeps it in `DELETE_REQUESTED` for 30 days**, as on Google, instead of
  removing it at once. Its ID cannot be reused until it is purged, and the purge drops the
  project's state in every service (#1216, #1537, #1540–#1543, #1550).
- Cloud Tasks and Cloud Scheduler accept `oidcToken` and `oauthToken` and send a locally minted
  token, where v0.1.0 refused them (#1243).

#### Cloud Run

- Service accounts and Secret Manager, in-memory, Cloud SQL, NFS and Cloud Storage volumes are now
  mapped (#1297, #1346, #1448, #1474). VPC access, encryption keys, binary authorization,
  `executionEnvironment` and `sessionAffinity`, which v0.1.0 refused, are stored and returned but
  have no effect (#1346).
- A revision that fails at startup is reported after 240 s rather than Knative's 600 s (#568).

#### Other services

- Secret Manager refuses the create fields it would otherwise drop, compares request etags, and
  verifies and returns payload CRC32C (#580). Each version is now a Kubernetes Secret of its own,
  so `AddSecretVersion` no longer fails past about 900 versions (#1596).
- Cloud Tasks keeps `dispatch_deadline`, honours `max_retry_duration`, and refuses the fields it
  would drop (#578).
- A Pub/Sub instance needs a CLI with the embedded Linux binaries (`make build` or a release),
  because its front runs from the storage image (#873).
- Google's Java emulators (Pub/Sub, Firestore and Datastore) run with a 1 GiB heap by default, and
  the BigQuery emulator with a 512 MiB `GOMEMLIMIT`. A Pub/Sub emulator whose heap fills exits and
  restarts (#1603, #1618).
- `--mode ephemeral` also applies to Cloud KMS and Secret Manager state (#481, #483).
- REST error bodies carry Google's `status` field and `details` (#1470).

#### CLI

- `cloudburrow reset` resets a running instance through its admin API (#586).
- `up` creates the managed namespace whenever the cluster is up (#571), waits until Cloud SQL
  accepts a login and the cluster's DNS answers before it reports ready, and starts independent
  phases at the same time. A warm start takes about 29 s on macOS/arm64 (#1262, #1588, #1610,
  #1781).
- `stop`, `env` and `status` find an instance by its PID and start time, not by process name
  (#820). `env` and `status` report only the services and ports the named instance runs (#630,
  #652, #863).
- A CLI built without the embedded Cloud Storage server is refused before `up` creates a cluster,
  and `doctor` reports what is embedded (#686). `doctor` names the container engine and warns on
  unsupported ones (#712).
- CI builds the embedded BigQuery emulator once per workflow run, or restores it from a cache
  (#1087). `make bigquery-binaries BQENGINE_FLAGS=-prebuilt` checks binaries built elsewhere.

### Fixed

#### BigQuery

- Requests no longer slow down with every job: the emulator read every job it had run, with its
  results, on every request (#1086, #1103, #1133, #1661).
- The SQL engine no longer panics past 2 GiB of memory, and no longer grows by a whole builtin
  catalog at each DDL statement. An engine that does fail is restarted at once, and the front
  forgets the jobs it lost (#989, #1016, #1017, #1057, #1091).
- Table functions, nested RECORDs, NaN and ±Infinity values, non-UTF-8 BYTES, `-0`, INTERVAL signs
  and comparisons, NULL arguments, GEOGRAPHY text and `TO_JSON_STRING` now give BigQuery's answers
  (#900, #1047, #1066, #1075–#1077, #1109, #1116, #1119–#1121, #1126–#1128).
- CSV loads no longer drop the first row as a header, failed jobs no longer read back as
  succeeded, `IF NOT EXISTS` of an existing table does nothing, and a failed script keeps the
  statements before the failing one, as BigQuery does (#931, #932, #934, #935, #955).
- A table ID used in two datasets reads the right table in `tabledata.list`, queries and the
  Storage Read API (#1015, #1032, #1046).
- A `jobs.insert` with no `jobReference` no longer answers 500, and `jobs.list` honours its
  paging and filters (#972, #973).
- Storage Read API reads of REPEATED columns no longer crash the emulator, and Avro reads work in
  a project whose ID has a hyphen (#1095, #1098).

#### Pub/Sub

- `/admin/reset` deletes schemas too (#889).
- Updated expiration policies survive a restart of the front, REST answers over 4 MiB pass
  through, and `GetSubscription` no longer counts as activity (#898, #926, #1039).
- The emulator's memory no longer rises for the length of a long run (#1603, #1619).

#### Cloud Run and Cloud Tasks

- A task deleted during its dispatch attempt is no longer recreated by the attempt (#656).
- Console actions on Cloud Run no longer dial a fixed port when the Run port is OS-assigned
  (#646).

#### Other services

- Bigtable answers `UNIMPLEMENTED` for the 15 RPCs that crashed its emulator pod (#1206), and its
  pod no longer reaches `dl.google.com` on every start (#611).
- Cloud KMS and Secret Manager no longer hold their lock across one Kubernetes read per record, so
  requests no longer time out on a large instance (#1232).
- Cloud SQL in ephemeral mode keeps a created instance's data across a server restart (#1740).
- Resource Manager's `Stop` waits for every call's handler, so a call's trace span is no longer
  cut short (#1803).
- `up` over a cluster made before the builtin Cloud Storage server removes the old server instead
  of crash-looping (#780).
- The console loads back an archive that its own **Save state** wrote, over the upload limit
  (#1250), and long monospace text no longer pushes a 320 px page sideways (#1637).

#### CLI, networking and releases

- Port forwarding replaces a dead tunnel on evidence rather than on one client's failure, retries a
  launch that does not take, and notices a pod that restarted its containers (#526, #566, #572).
- **The setup-cloudburrow action works with the CLI it installs** (#679): it passes only the flags
  that CLI has, finds its own files outside composite actions, and is self-tested against the
  newest release. [docs/ci.md](docs/ci.md#which-cli-versions-an-action-ref-supports) has the table.
- **Both Linux release archives are statically linked** (#714): every release CLI is built with
  `CGO_ENABLED=0`, and the release checks it. HTTP targets under `.localhost` are dialled on
  `127.0.0.1` without a DNS lookup.
- With Cloud Run, `up` checks that the kind gateway can be bound, and otherwise names the engine
  and the workaround (#712). See [docs/install.md](docs/install.md#container-engines).

### Security

- Every `/admin` route needs the instance's admin token, because a cluster workload on Docker
  Desktop can reach the host's loopback ports (#553).
- Every HTTP listener refuses a `Host` that is not an IP address, `localhost`, a `.localhost`
  name or one of the instance's own names, answering 421. This is the DNS-rebinding defence, and
  it covers the tunnels to the HTTP emulators and the fronts in their pods (#676, #725, #1587).
- **Every listener now checks `Origin`** (#1586): a browser request from an origin that is not
  loopback, the listener's own, or one named with `--cors-allow-origin` is 403 with no CORS
  headers. A cross-site form post could otherwise create resources. The Pub/Sub and Firestore
  emulators' permissive CORS is blocked at the tunnel. Cloud Storage's JSON API, which allowed any
  origin as Google's does, follows the same rule (#677). A request with no `Origin`, as the SDKs and
  `curl` send, is unaffected.
- A discovered `cloudburrow.json` can no longer set `allowRemote` or a non-loopback
  `bindAddress`, and a directory's config file and hooks run only once that directory is trusted
  (#598).
- `up` tightens an existing instance directory to mode 0700, and log redaction now covers quoted
  JSON tokens, bare bearer tokens and PEM key bodies (#1586). [docs/security.md](docs/security.md)
  describes the model and each check.
- The terminal's WebSocket is refused to any other origin, host or remote peer (#781).
- Releases build with Go 1.27.4, and dependencies with known reachable vulnerabilities were updated
  (#264, #1175, #1178).

### Removed

- The community Cloud Storage emulator v0.1.0 ran, replaced by CloudBurrow's own server (#519).
- Google's product icons in the console, replaced by CloudBurrow's own marks (#684).
- About 59 MB of committed build output; `make build` and releases build the embedded servers
  (#585).
- Java from the support matrix's planned languages: no Java suite is planned (#718).

## [0.1.0] - 2026-09-25

The first release: a local Google Cloud emulator that runs in a local Kubernetes cluster and is
driven through the official Google Cloud SDKs. What is and is not supported is recorded per
operation in [docs/compatibility.md](docs/compatibility.md) and summarised in
[docs/status.md](docs/status.md).

### Added

- **A CLI and an owned local Kubernetes cluster**: `up`, `status`, `stop`, `reset`, `delete`,
  `env`, `doctor`, `logs`, `wait`, `up --detach`, `status --format json`, `diagnose`,
  `gcloud-setup` and `gcloud-teardown`, and `cloudburrow terraform` (#53, #54, #89, #293, #305,
  #325, #326, #327, #331).
- **Services started by default**: Cloud Storage, with notifications to Pub/Sub (#95); Pub/Sub,
  on Google's emulator; Cloud Tasks with HTTP dispatch and retry (#66, #67, #322); Cloud Run v2
  backed by Knative Serving, including revisions and updates (#68, #299, #300); and Secret
  Manager over gRPC and JSON, backed by Kubernetes Secrets (#93, #94).
- **Opt-in services** with `--services`: Firestore, Datastore, Bigtable and Spanner on Google's
  emulators (#75, #371); BigQuery on a community emulator (#323); Cloud SQL for PostgreSQL and
  MySQL data planes (#122, #297, #311); Memorystore on Valkey (#296); Cloud Scheduler (#302);
  Cloud Logging write and read (#304); and Cloud KMS with symmetric keys, Encrypt and Decrypt,
  the version lifecycle and REST transcoding (#385–#424). Cloud KMS is not a security boundary.
- Resource Manager v1 and v3 projects (#298, #301), local credentials and a GCE metadata server
  (#92), and IAM Credentials token generation from the local signer (#303).
- IAM policies stored, never enforced, on Secret Manager secrets and Cloud Tasks queues
  (ADR-0006, #308, #365, #366).
- **Vertex AI on local runtimes**: a `generateContent` subset verified through the official
  `genai` SDK (#105, #106) and custom prediction containers (#90).
- **A local web console** laid out after the Google Cloud console, with detail pages, actions
  where the backend supports them, SQL and query editors, Kubernetes views, metrics, a Logs
  Explorer and a request log (#97 onward).
- An admin API for reset, seed and event inspection (#70, #317, #319, #320, #334), state save
  and load (#338, #340), lifecycle hooks (#333), fault injection (#306), request logging
  (#314), request metrics (#341) and OpenTelemetry traces (#313, #379).
- The setup-cloudburrow GitHub Action (#330), `env --format terraform` and
  `--format docker-compose` (#332).
- SDK compatibility suites for Go and Python (#56, #335), and per-service API coverage generated
  from the proto surface (#337).
- Release packaging: archives for macOS and Linux on arm64 and amd64 with build attestations,
  one `checksums.txt`, a Homebrew formula and an installer that refuses an archive it cannot
  verify (#71, #324).

[Unreleased]: https://github.com/cloudburrow/cloudburrow/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/cloudburrow/cloudburrow/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0
