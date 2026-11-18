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
| Cloud Run | 5 | 2 | 1 | 1 | 1 |
| Cloud Scheduler | 1 | 0 | 0 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 5 | 0 | 1 | 3 | 1 |
| Secret Manager | 1 | 0 | 0 | 0 | 1 |
| Resource Manager | 2 | 0 | 0 | 2 | 0 |
| IAM custom roles | 10 | 2 | 1 | 7 | 0 |
| Vertex AI | 1 | 0 | 1 | 0 | 0 |
| BigQuery | 19 | 0 | 6 | 13 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 5 | 0 | 1 | 3 | 1 |
| Cloud SQL, Memorystore | 6 | 1 | 4 | 1 | 0 |
| Functions, credentials, tooling | 1 | 0 | 1 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **62** | **5** | **18** | **33** | **6** |

## Cloud Storage

Closed: `objects.bulkRestore` (#1279), `buckets.operations.list` filters (#1280), `channels.stop`
and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up`
instance (#1290), the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), the gRPC
`google.storage.v2` API (#1321), and its customer-supplied encryption keys, ACLs, managed-folder
IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter` (#1345),
`objects.viewFullContext` (#1700), `softDeletePolicy.hardDeletePause` (#1702). ACLs
and object IAM on the JSON API are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Notifications: the server with no Pub/Sub refuses a configuration 501, as it has no topic to verify or deliver to (#1444) (#1505); rechecked (#1521): the notifications insert reference lists no error, only the `storage.buckets.update` permission, and the Pub/Sub notifications guides describe a missing topic or publisher grant only as a delivery failure, so no documented error replaces the 501 (#1577) | no emulator support (needs Pub/Sub) | S |
| UNVERIFIED error codes (a non-empty bucket delete, a repeat retention lock, managed-folder and folder refusals, batch limits, CORS on JSON endpoints, the hard delete pause refusals (#1715) and others; [storage.md](coverage/storage.md)) (#1445) | not documented by Google | S |

## Pub/Sub

Closed: `DetachSubscription`, `UpdateSnapshot` and Protocol Buffer schemas, served by CloudBurrow's
Pub/Sub front (#1406), and IAM on topics, subscriptions, snapshots and schemas, stored and never
enforced (#1367).

| Gap | Why open | Size |
|---|---|---|
| The front's messages for a refused `.proto` definition or message, and for the calls Google documents no answer to on a detached subscription (`FAILED_PRECONDITION` for a snapshot of it or a seek to a snapshot, `INVALID_ARGUMENT` for an immutable field), are its own (#1445; built in #1446) | needs live GCP: Google's messages not measured | S |
| UNVERIFIED error codes: subscription expiration, exactly-once and label refusals ([pubsub.md](coverage/pubsub.md); an invalid label's message is #982) (#1445) | not documented by Google | S |

| Delivery to an App Engine app (#1447) (#1506): with `--appengine-url` a local server stands in for the app and gets each attempt as `AppEngineHttpTarget` documents it (built in #1506), else every attempt fails `NOT_FOUND` (HTTP 404) (#1521) (#1577) | no emulator support (no App Engine runtime runs locally, and Google documents no mapping to Cloud Run, so the stand-in is the developer's own server) | — |
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
| Worker pools: an `AUTOMATIC` pool runs its `minInstanceCount` and never grows toward `maxInstanceCount` on its `cpuScaling.cpuUtilization` target: nothing reads pod CPU in the kind cluster (#1789; the fields are served over REST since #1788) | not built yet | M |
| Services: `template.scaling.cpuUtilization` above 0, and `concurrencyUtilization` 0.0, are refused UNIMPLEMENTED (Knative's KPA scales on concurrency only); a `MANUAL` count that leaves a serving revision with no instances (`manualInstanceCount: 0`, a disabled service) is refused UNIMPLEMENTED; the console does not yet offer service-level scaling (#1790 follow-up) | not built yet | M |
| Worker pools: a `minInstanceCount` above a set `maxInstanceCount` is INVALID_ARGUMENT, an unset `cpuUtilization` sets no CPU threshold, and `manualInstanceCount` is ignored (kept) under `AUTOMATIC` (#1789) | not documented by Google | S |
| A stale `etag` on a job is `ABORTED`; a failed `RunJob` is `FAILED_PRECONDITION`; `CancelExecution` of an ended execution is `FAILED_PRECONDITION` (#1445) | needs live GCP | S |
| Cloud Build: BuildTriggers whose source names a hosted git service (GitHub, GitLab, Bitbucket, Cloud Source Repositories) or a `filename` in one are refused 501 with the reason; Secure Source Manager repositories run their `.cloudbuild/triggers.yaml` triggers on push (#1521), in the console and with gcloud and Terraform (#1527); Developer Connect connections to this instance's Secure Source Manager and their git repository links are served, and a triggers-file trigger runs through its `devConnectGitRepositoryLink` (#1546), while a BuildTrigger's `developerConnectEventConfig` and Cloud Build v2 repositories stay refused, as Google documents no Secure Source Manager type for either (#1546); rechecked (#1521): cloudbuild v1.35.0's protos still give a v2 `Connection` only GitHub, GitHub Enterprise, GitLab and Bitbucket configs, which need those hosted services and egress, and Cloud Source Repositories (`triggerTemplate`), which Google closed to new customers on 2024-06-17, is not served, so those triggers stay refused; Pub/Sub triggers are built (#1506). Artifact Registry: every format and method is served (#1531, #1539), and remote `DOCKER`, `PYTHON`, `NPM` and `MAVEN` repositories over another of the instance's repositories (`commonRepository.uri`) pull through and cache (#1546), while remote repositories over public or custom upstreams, and remote `APT` and `YUM` ones, are refused, as egress is off (#1546) (#1577) | no emulator support | M |

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
| A BigQuery sink's `export_errors` column types and `schemaErrorDetail` text, `Any` fields but `serviceData` (left out), and the sink error codes but `topic_not_found` are CloudBurrow's (#1451; `export_errors` and `protoPayload` are built; `Duration` fields are `FLOAT` seconds, as Google's Log Analytics migration guide shows for `httpRequest.latency`, #1506); rechecked (#1521): "BigQuery schema for logs" names the error table's fields without types and says only that `schemaErrorDetail` "contains the complete error message returned by BigQuery"; the one `Any` mapping it states is audit logs' `request`, `response` and `metadata` as JSON strings in `*Json` columns, so the rest stays UNVERIFIED. Every sink destination Google lists (a log bucket, a project, BigQuery, Cloud Storage, Pub/Sub) is delivered to; intercepting sinks (`intercept_children`) are built (#1521), but for ListSinks' `in_scope` filters, which loggingpb's ListSinksRequest lacks (#1577) | not documented by Google (the export page lists the `export_errors` columns without types, and `schemaErrorDetail` only as BigQuery's message); needs a live sink | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by the sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| REST: `alt` other than JSON, and `fields`/`$fields`, are `UNIMPLEMENTED` (#1445) | not documented by Google | S |
| UNVERIFIED error codes ([kms.md](coverage/kms.md)) (#1445) | not documented by Google | S |
| Project-number parents are refused; whether Google accepts them, and whether an empty key ring can be deleted (#1445) | needs live GCP | S |
| `key_access_justifications_policy` is enforced (#1506): encrypt, decrypt and sign fail under a policy with no reasons, as documented, or without `CUSTOMER_INITIATED_ACCESS`, a local call's reason; the refusal's code and text (`FAILED_PRECONDITION`) are CloudBurrow's, and project and organization default policies are not served (#1452) (#1521) (#1577) | not documented by Google | S |
| Single-tenant HSM (#1469): instances and proposals never pass through their transitional states, since nothing is provisioned (HsmManagement and HSM_SINGLE_TENANT keys are served since #1469; HSM since #1452, without an HSM or an attestation; EXTERNAL and EXTERNAL_VPC since #1469, the instance acting as the EKM). Built in #1521, as kms v1.35.0's protos and the single-tenant guide define them: `upgrade_key_trust` proposals through the quorum approval, AES_WRAPPING (`AES_256_KWP`) keys, `trusted_wrapping_enabled`/`hsm_trusted`, HSM_SINGLE_TENANT import jobs, and `Import`/`ExportTrustedKeyWrappedCryptoKeyVersion` with RFC 5649 AES-KWP; the asymmetric material is PKCS #8 DER as for ordinary key import (UNVERIFIED: nothing states its encoding under trusted wrapping), and the key portability requirement is CloudBurrow's reading (UNVERIFIED). Still refused 501 with the reason: trusted re-import into an existing version, and ML-KEM keys, whose material encoding Google documents for no import (#1577) | no emulator support (no HSM); the asymmetric encoding not documented by Google | S |

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
and never enforced (#1368); IAM Conditions on their policies, evaluated by Firestore with MongoDB
compatibility's and Memorystore's IAM checks, and edited in the console (#1742); every
attribute Google's reference documents for a role binding, conditions on the policies of the
services Google lists as allowing them (and the same refusal elsewhere), and Memorystore's
per-instance evaluation (#1745); `api.getAttribute` with a map default, conditions on the
Bigtable and log view Permissions tabs, and official-SDK compat for Cloud Run, BigQuery, Bigtable
and Memorystore (#1748).

| Gap | Why open | Size |
|---|---|---|
| IAM Conditions: the INVALID_ARGUMENT messages for a condition on a version 1 policy, a missing title or expression, an expression that does not compile, and the 12-operator and 20-condition limits are CloudBurrow's (Google publishes only `Policy cannot contain both conditions and condition hashes`), and so is the hash in `ROLE_withcond_HASH`, the INVALID_ARGUMENT a service that does not allow conditions answers one, and Cloud Storage's 400s for a condition without uniform bucket-level access and for disabling it under one (built in #1742, #1745) (#1745) | not documented by Google | S |
| An app-enabled folder's management project is `google-mp-{folder number}`, shaped as the discovery document's one example: the project ID and display name Google assigns are not documented, and the APIs it enables on the project are not modelled (built in #1532) (#1445) | not documented by Google | S |

## IAM custom roles

Closed: `projects.roles` and `organizations.roles` (create, get, list, patch, delete, undelete),
`permissions.queryTestablePermissions`, custom roles in bindings and in every permission check,
gcloud iam roles and Terraform's custom role resources (#1743). Google's predefined roles through
`GetRole` and `ListRoles`, `QueryGrantableRoles` for projects, folders and the organization, the
console's predefined roles, a deleted role's bindings removed when its deletion completes, and the
codes Google's samples catch for reusing a deleted role's ID, deleting a deleted role and undeleting
a live one (`FAILED_PRECONDITION`) (#1746). Service accounts, their keys and IAM policies, IAM
Credentials' checks and signBlob, key-file sign-in, gcloud iam service-accounts and Terraform's
service account resources (#1747). Impersonation through any predefined role and through folder and organization grants, a deleted account's bindings as `deleted:serviceAccount:...?uid=` and their restoration on undelete, the account's public certificate URLs, PKCS#12 key files and RSA 1024 keys (#1750). The IAM API's deprecated `SignBlob` and `SignJwt` with the caller's permission check, `QueryAuditableServices`, `LintPolicy`'s eleven validation units, and a deleted account's bindings in Secret Manager's, Cloud KMS's, Cloud Tasks', Cloud Storage's, Pub/Sub's, Bigtable's, Spanner's, BigQuery tables', Logging views', Cloud Run's and Artifact Registry's policies (#1751).

| Gap | Why open | Size |
|---|---|---|
| `LintPolicy` (#1751): Google's page shows one example per unit, so these are CloudBurrow's: HasOnlyListConstCheck's offset (CEL's position of the non-constant element, 62 for the page's example, where the page gives 59, which is no node of that expression); a compile error other than the page's (the ANTLR message for a missing `)` or a trailing token, and for a type-check error the checker's own text after `ERROR: Parse expression:`); DateTimeCheck's message for a malformed timestamp and for an always-true comparison; EffectiveTimeRangeCheck's message for comparisons joined by `||` that cover a function's whole range (`results in full ranges`), and that it needs two comparisons of one function; RestrictedAttributesCheck warns of `request.auth.access_levels` alone, the one attribute the page names; results of one severity are in the page's unit order. The caller's permissions on the resource are not read | not documented by Google | S |
| A deleted service account's bindings (#1751): Bigtable authorized views and backups, Spanner backups, BigQuery routines, and Cloud Run services and worker pools are rewritten by the same code (in the same stores) as the resources `TestIAMDeletedServiceAccountInEveryServicesPolicies`, `TestIAMDeletedServiceAccountInEmulatorFrontsPolicies` and `TestIAMDeletedServiceAccountInRunShardPolicies` check, but no compat test reads them yet | not built yet | S |
| A BigQuery dataset's access entries are not rewritten when an account they name is deleted, undeleted or purged: they are kept by the BigQuery engine, not the front, and how Google shows a deleted account in a dataset's access list (`userByEmail` or `iamMember`) is not yet measured against Google's documentation (#1751) | not built yet | S |
| `QueryAuditableServices` (#1751) answers Google's documented audit-log service list whatever the project enables, which Google does not say it filters | not documented by Google | S |
| A service account's certificate URLs (#1750): Google's body for an unknown account (404 `NOT_FOUND` here), and whether disabled keys or a disabled account's keys are listed (not here); the codes for an unknown private key type or key algorithm (`INVALID_ARGUMENT` here) | not documented by Google | S |
| Service accounts (#1747): the per-project account quota (100 here; Google's "varies depending on the project"), the code for a disabled account in IAM Credentials (`FAILED_PRECONDITION` here), whether self-impersonation is checked before the permission, the messages for an invalid account ID, display name or description, and the code for deleting a system-managed key | not documented by Google | S |
| `QueryGrantableRoles` and `QueryTestablePermissions` for resources other than a project, folder or the organization (a bucket, a topic, a service account): 501; Google's docs say on which lowest-level resources only 254 of the 2,423 predefined roles can be granted, and map no permission to a resource type (#1746) | not documented by Google | M |
| The basic roles' (`roles/owner`, `editor`, `viewer`, `admin`, `writer`, `reader`) included permissions, and their API descriptions: Google's docs point to the console for them, so they are served without permissions; a predefined role whose page gives a long description is served its first paragraph; `GetRole` of an unknown predefined role is `NOT_FOUND`, and `UpdateRole`, `DeleteRole` or `UndeleteRole` of a predefined one `UNIMPLEMENTED` (#1746) | not documented by Google | S |
| The code for a 301st role (`FAILED_PRECONDITION`), which permissions beyond the organization and folder ones a project role may not hold, and a testable permission's `stage` (#1743); exactly when, within "7 to 14 days", a deleted role stops counting toward the 300 (here at 7), and when, in the 30 days after, its bindings go (here at 37) (#1746) | not documented by Google | S |
| Terraform's `google_iam_testable_permissions` data source: provider 8.x posts to `https://iam.googleapis.com/v1/permissions:queryTestablePermissions`, built in rather than read from `iam_custom_endpoint`, so it cannot reach an instance (#1746) | no emulator support | S |

## Vertex AI

Closed: `PredictionService.EmbedContent`, over gRPC and REST `:embedContent` (#1244);
`PredictionService.StreamGenerateContent` over gRPC, and an official-SDK test of `GenerateContent`
over gRPC (#1257); `RawPredict` on a publisher model (#1284); `StreamRawPredict`, `DirectPredict`,
`ServerStreamingPredict`, `StreamingPredict` and `StreamDirectPredict` (#1285); `DirectRawPredict`,
`StreamDirectRawPredict` and `StreamingRawPredict`, relayed to a model's gRPC server on its `h2c` port (#1477); `Explain` with integrated gradients, XRAI or
examples, now `INVALID_ARGUMENT` on a custom container as Vertex documents (they are for TensorFlow
models only; Sampled Shapley, the custom-container method, is served) (#1454); `EmbedContent` with images, on
EmbeddingGemma 2's community ONNX conversion (#1454); with WAV audio, and `file_data` `gs://` parts read
from the instance's Cloud Storage (#1651, open for the rest); with MP3 audio (#1721); with HEIC and
HEIF images and an animated WebP's first frame (#1724); with AVIF images (#1729).

| Gap | Why open | Size |
|---|---|---|
| `EmbedContent` on `embeddinggemma-2-onnx-community` embeds text, PNG, JPEG, WebP (an animated one as its first frame), BMP, HEIC, HEIF or AVIF (#1729, #1736: 8 to 12 bits up to 16384 × 16384) images (an image sequence as its first frame), WAV or MP3 audio (MPEG-2.5 too, #1734), MP4 and MOV video in H.264, H.265, VP9 (every profile since #1739) or AV1 (#1722, #1731) and PDF (#1723), inline or as `file_data` from the instance's Cloud Storage (#1454, #1651, #1721, #1724), with a video's AAC-LC, HE-AAC, MP3, PCM, Opus, AC-3 or E-AC-3 (#1739) audio track under `audio_track_extraction` (#1732), but not HE-AACv2 parametric stereo with 34 bands, IPD/OPD or mixing procedure B: those are `INVALID_ARGUMENT` naming the field. VP9 profile 0 is decoded with a copy of `github.com/thesyncim/govpx` (`third_party/govpx`), profiles 1 to 3 with `third_party/govpx/vp9hbd`, written here from the VP9 specification; AV1 with `github.com/gen2brain/gav1d` (#1731). AC-3 and E-AC-3 are decoded by `internal/ac3` (written here from ATSC A/52), HE-AAC by SBR and PS added to `third_party/aac-go` (#1739); the full parametric stereo tools (34 bands, IPD/OPD, mixing procedure B) need ISO/IEC 14496-3's normative tables, which no permissively licensed source prints, and no encoder at hand emits them (#1739). `document_ocr` reads a PDF's own text, and a scanned page's with Tesseract OCR (#1733; Verified with the official client 2026-11-17). Google's doc re-read 2026-11-17 (it documents "MP3, WAV" and 180 seconds of audio a request, and no audio file size): https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings (#1651, #1721, #1722, #1723, #1724, #1736; the video and PDF evaluation is in [upstream-evaluation.md](upstream-evaluation.md)) | no emulator support (full parametric stereo) | L |

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
| A STRUCT field's `data_policies` is 501, as Google documents data policies on a column only (#1456; a dry run of `CREATE VIEW` of a view that exists is 409 `duplicate`, the statement's own error, since #1456's last part); smart tuning's rewrite of a base-table query to read a materialized view is not built: the results are the same, and whether BigQuery chooses a view is its cost estimate (`rejectedReason` `COST`), which Google does not document (#1456) | not documented by Google | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (JobConfigurationLoad read again 2026-10-17: V1 "Support alphanumeric + underscore characters and names must start with a letter or underscore", V2 "Support flexible column name", neither saying how a name still refused or two the same are named; Specifying a schema, read 2026-10-17, says "Duplicate column names are not allowed even if the case differs" and not how a map names two the same); a Parquet name with a period is 400 since #1547 ("Periods are not replaced by underscores in Parquet file column names when a column name character map is used") (#1457) (#1547) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting (into a `JSON` column the schema gives, mixed values load since #1457), `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501; schema-detect read again 2026-10-17 (a sample of "up to the first 500 rows", the header rule, generic names "such as string_field_1") documents no more of these (#1457) (#1547) | not documented by Google | S |
| CSV: a quote outside ISO-8859-1, a delimiter of several bytes or outside ASCII in UTF-16 or UTF-32 data (the reference converts a quote or delimiter "to ISO-8859-1" and splits on "the first byte", and says nothing of other encodings), a `maxBadRecords` load the emulator still fails on a value the front passes (a number written as a fraction into an INTEGER, a DATETIME or TIMESTAMP), `sourceColumnMatch` `NAME` with no row skipped (ISO-8859-1 delimiters and quotes, a quote's first byte, `skipLeadingRows` N and the header with `autodetect`, `NAME` after skipped rows, and, in #1457's second part, a delimiter's first byte, a delimiter or quote outside ASCII in UTF-8 data and an empty `STRING` with a `nullMarker` and, under `maxBadRecords`, a BOOL, DATE, TIME or number its column refuses as a bad record are built); JSON: a null field outside the schema without `ignoreUnknownValues` (the reference does not say whether a null is an "extra value"; under `ignoreUnknownValues` it is ignored since #1457), a `maxBadRecords` load the emulator still fails on a value the front passes (a value its column refuses is a bad record since #1457) (#1547) | no emulator support | M |
| Parquet: the refused columns left (an INT96 with a part of a microsecond, a DECIMAL that would round, DECIMAL to STRING, a NULL element or a LIST of lists with list inference); a NaN loads and unlisted types are 400 since #1457; the Parquet loading page (read again 2026-10-17) maps LIST, ENUM, DECIMAL and INT96 but says nothing of these values, so they stay 501 (#1457) (#1547) | not documented by Google | S |
| Partition decorators: `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination, a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, and through a partition a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order or a RECORD's fields changed, are 501, none documented by BigQuery (docs re-read 2026-11-17, still silent on each, so they stay 501: [managing-partitioned-table-data](https://docs.cloud.google.com/bigquery/docs/managing-partitioned-table-data) says only that a decorator can "write data to a specific partition" and that `__NULL__` and `__UNPARTITIONED__` can be exported; [streaming-data-into-bigquery](https://docs.cloud.google.com/bigquery/docs/streaming-data-into-bigquery) documents a decorator to "override the date inference" of a daily table only ("Streaming using a partition decorator is only supported for daily partitioned tables"), not a column-partitioned table, whose rows' partition is their column's; [managing-partitioned-tables](https://docs.cloud.google.com/bigquery/docs/managing-partitioned-tables) documents copying partitions (`bq cp`) and that `__NULL__` and `__UNPARTITIONED__` cannot be deleted, nothing of cloning, snapshotting or restoring one, nor of writing to `__UNPARTITIONED__`; [table-decorators](https://docs.cloud.google.com/bigquery/docs/table-decorators) names time decorators only; and schemaUpdateOptions name no rule for a write through a partition) (#1458) | not documented by Google | S |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type (#1445) | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) (#1445) | not documented by Google | S |
| A table restored from state saved before #1277 keeps no ingestion time: its rows' `_PARTITIONTIME` was never recorded (a table kept from before the engine had the stream-time column gets it since googlesqlite patch 0089, NULL on its old rows) (#1458) | no emulator support (the times were never recorded) | S |
| `PARTITIONS`' `last_modified_time`, `storage_tier` and region `TABLE_STORAGE`'s active and long-term bytes of a table older than 90 days are NULL for a partition whose last write the engine let go of before googlesqlite patch 0090 (which keeps every write record the change table lets go of, #1458) or, for an ingestion-time partitioned table, whose change table was made before patch 0050: those write times were never kept; `TABLE_STORAGE`'s physical byte columns are estimates, the uncompressed logical bytes, as BigQuery defines no compressed size (UNVERIFIED, docs re-read 2026-11-17: <https://docs.cloud.google.com/bigquery/docs/information-schema-table-storage> calls them "physical (compressed) bytes" and gives no way to compute them) (#1458) | no emulator support (no compressed sizes; write times lost before patch 0090) | S |
| Avro loads (built in #1457): a union of several types, a `map`, a recursive record, an array whose items may be null, a `decimal` made STRING by `decimalTargetTypes` and, for Avro and Parquet, a data file whose schema differs from its `referenceFileSchemaUri`'s (one of the reference's schema loads since #1457) are 501, as BigQuery does not document how it loads them (Loading Avro data's conversions, read 2026-10-17: a union of several types is "a RECORD with a list of nullable fields", their names not given; a map "a repeated RECORD that contains two fields: a key and a value", the entries' order not given, which goavro does not keep; recursive fields "are dropped — Only the first level of nesting is maintained", not saying which level is the first; arrays "containing only NULL types are ignored", nothing said of a NULL item); zstandard files load since #1547 (#1457) (#1547) | not documented by Google | S |
| `DATASTORE_BACKUP` loads are 501: a `DATASTORE_BACKUP` load reads a managed export's `KIND.export_metadata` and its legacy Datastore entities, whose bytes Google does not document (Loading data from Firestore exports, read 2026-11-17: "Firestore and Datastore share an export format"), and CloudBurrow's Firestore export writes a JSON `.export_metadata` and Firestore `Document` records (measured, `internal/firestoreadmin/transfer.go`), so neither Google's exports nor its own load faithfully. Hive-partitioned CSV by `autodetect` with no schema, with `sourceColumnMatch` `NAME` or a time format, or into a table with no key column, is 501, the page documenting no CSV specifics. Hive-partitioned ORC with a schema in the job is 501. Built since #1547 and #1713: JSON, CSV (with a schema), Parquet and Avro (with or without a schema that is the file's) and ORC (with none) hive-partitioned loads, `jsonExtension` `GEOJSON`, and manifest `fileSetSpecType` loads. Of `ORC` loads (built in #1713), a `decimal` made STRING by `decimalTargetTypes`, files whose schemas differ (BigQuery takes the alphabetically last file's), a writer's time zone the machine's time zone database lacks, and a dry run are 501, and a TIMESTAMP is read as its writer's wall-clock time in UTC (UNVERIFIED, docs re-read 2026-11-17: https://docs.cloud.google.com/bigquery/docs/loading-data-cloud-storage-orc) (#1457) (#1547) (#1713) | no emulator support | M |
| BQML: `LINEAR_REG`, binary and multiclass `LOGISTIC_REG`, `KMEANS`, `PCA`, explicit `MATRIX_FACTORIZATION`, `BOOSTED_TREE_REGRESSOR`, `BOOSTED_TREE_CLASSIFIER`, `RANDOM_FOREST_REGRESSOR`, `RANDOM_FOREST_CLASSIFIER`, `DNN_REGRESSOR`, `DNN_CLASSIFIER`, `AUTOENCODER` (#1548), `DNN_LINEAR_COMBINED_REGRESSOR`, `DNN_LINEAR_COMBINED_CLASSIFIER` (#1562) and `ARIMA_PLUS` are trained by the front, with `TRANSFORM`, ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO, ML.TRAINING_INFO, ML.CONFUSION_MATRIX, ML.ROC_CURVE, ML.CENTROIDS, ML.PRINCIPAL_COMPONENTS, ML.PRINCIPAL_COMPONENT_INFO, ML.RECOMMEND, ML.FEATURE_IMPORTANCE, ML.FORECAST, ML.EXPLAIN_FORECAST, ML.DETECT_ANOMALIES, ML.ARIMA_EVALUATE, ML.ARIMA_COEFFICIENTS, ML.EXPLAIN_PREDICT, ML.GLOBAL_EXPLAIN and models.* (#1465, #1481, #1504; ARIMA_PLUS's seasonal decomposition, spikes and dips, step changes and forecast limits #1518), and the preprocessing functions ML.BUCKETIZE, ML.POLYNOMIAL_EXPAND, ML.FEATURE_CROSS, ML.STANDARD_SCALER, ML.MIN_MAX_SCALER, ML.MAX_ABS_SCALER, ML.IMPUTER (mean), ML.DISTANCE, ML.LP_NORM and ML.NORMALIZER, in a query and in TRANSFORM (#1504), and ML.QUANTILE_BUCKETIZE, ML.ROBUST_SCALER, ML.NGRAMS and the vocabulary functions ML.LABEL_ENCODER, ML.ONE_HOT_ENCODER, ML.MULTI_HOT_ENCODER, ML.BAG_OF_WORDS and ML.TF_IDF (#1548); a DNN's FTRL, CRELU, class weights and auto class weights, an autoencoder's l1_reg_activation, and ML.HASH_BUCKETIZE since #1562, and a DNN's explain functions and `enable_global_explain` are 400 as BigQuery lists no explanation for one (#1562); sampled boosted trees, `ARIMA_PLUS_XREG` and ARIMA_PLUS's holiday regions (Google's holiday calendar, not in CloudBurrow), implicit matrix factorization, AutoML (Vertex AI's hosted model search), imported TensorFlow and TensorFlow Lite models (no runtime in any build) and imported ONNX models in the default build (the opt-in onnx build, `make build-onnx`, imports them and runs ML.PREDICT on its ONNX runner since #1711; imported XGBoost models run in every build on the pure-Go `internal/xgbmodel` since #1711) and remote models (no BigQuery Connection API; Google-hosted endpoints) are 501 with the reason measured in #1562 (#1711) | no emulator support: training Google does not document, or Google's infrastructure | L |
| Table options: `storage_uri` with no connection is kept only, no files written: Google's Iceberg managed tables guide says "you must" specify the connection with `WITH CONNECTION` (or `WITH CONNECTION DEFAULT`), and documents no answer for a `storage_uri` without one (docs re-read 2026-10-17: https://docs.cloud.google.com/bigquery/docs/iceberg-tables, https://docs.cloud.google.com/bigquery/docs/default-connections); with a connection a table for Apache Iceberg since #1427, the formats defaulted and `WITH CONNECTION DEFAULT` since #1456; what #1426 and #1418 leave is in the rows below (#1456) (#1577) | not documented by Google | S |
| What #1426, #1455, #1482, #1502 and #1517 leave of `privacy_policy` (#1517 built: two `SELECT WITH AGGREGATION_THRESHOLD` in one query, a read with no clause in a subquery, CTE or view (400), `JOIN_ANY` met by an outer join and `JOIN_ALL` alongside one by each `join_condition`'s wording, a `NATURAL` join (400, a syntax error), a DP aggregate inside an expression, evaluated on the noisy values; the UNVERIFIED choices in docs/compatibility.md): a read of an aggregation threshold or join restriction table or view in a subquery or CTE outside its own `SELECT WITH AGGREGATION_THRESHOLD`, or through another view, in a query with a privacy clause; of differential privacy, `COUNT(DISTINCT)` and `max_rows_contributed` are 501: documented by neither BigQuery (analysis-rules, query-syntax#agg_threshold_clause and #dp_clause, aggregate-dp-functions, the join operation) nor GoogleSQL (read 2026-10-17) (#1589) | not documented by Google | M |
| `ALTER TABLE` of a foreign key to another project's table is 501: an instance serves one project, so there is no table of another project to reference (#1456) (#1577) | no emulator support (one project per instance) | S |
| `ALTER TABLE` in a dry run of a script that reaches it inside a `BEGIN` block, `LOOP`, `REPEAT` or `CASE`, or after a `CREATE TEMP TABLE` ("Dry runs of `CREATE TEMP TABLE` statements aren't supported", with no answer documented; the 501's message names it since #1456's last part), is 501: BigQuery's dry-run rules name only DDL, `CALL`, `EXECUTE IMMEDIATE`, and `FOR`, `IF` and `WHILE`, where the dry run stops (docs re-read 2026-10-17: https://docs.cloud.google.com/bigquery/docs/multi-statement-queries); one stopping at the `ALTER TABLE`, or before it at an `IF`, `FOR` or `WHILE`, is answered since fix/last-not-built (#1456), and before it at a `CALL` or `EXECUTE IMMEDIATE` since #1612, and `ALTER TABLE` in legacy SQL is 400 `invalidQuery` with CloudBurrow's own message (UNVERIFIED) (#1456) (#1577) | not documented by Google | S |
| A script's dry run that stops at a `CALL` or an `EXECUTE IMMEDIATE` before an `ALTER TABLE` refuses a missing procedure, its arguments' number, an `OUT` argument that is not a variable, an argument of another type, and an SQL expression that is not a `STRING`, with CloudBurrow's own messages (the multi-statement queries guide names the checks, not their messages); that a refused action of an `ALTER TABLE` of several leaves the earlier ones undone is read from the DDL reference failing the statement, not measured (#1612) (#1445) | not documented by Google | S |
| Project configurations: `ALTER PROJECT` / `ALTER ORGANIZATION` inside a `BEGIN` block, `LOOP`, `REPEAT` or `CASE` of a script in a dry run is 501, BigQuery's dry-run rules not naming those blocks (one inside an `IF`, `FOR` or `WHILE` is answered, the dry run stopping before it, since fix/last-not-built; docs re-read 2026-10-17: https://docs.cloud.google.com/bigquery/docs/multi-statement-queries); `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows, which Google does not document; `default_kms_key_name` reaches every table made (a script's and an upload load's since #1456) (#1456) (#1577) | not documented by Google | S |

## Spanner, Bigtable, Firestore, Datastore

Closed: Datastore structured aggregations and Firestore `PartitionQuery` (#1405); Spanner `AddSplitPoints` and `FetchCacheUpdate`, Bigtable change streams (with garbage collection and `DropRowRange` records) and the session data plane, and Firestore pipelines' stages and functions (#1523, #1613); Firestore with MongoDB compatibility's wire protocol, SCRAM authentication with the Admin API's user credentials, CRUD, queries, cursors, aggregation and indexes (#1703), and its transactions, `explain`, positional operators, `$text`, `$lookup` pipelines, TLS, IAM checks and the Standard edition guard on the `Write` stream (#1738), the SCRAM user's binding with its IAM condition naming the database (#1742) and custom roles (#1743); Firestore's Standard edition guard on the browser's WebChannel `Write` stream (#1744); Bigtable SQL's `UNPACK`, window functions, `STRUCT` constructors and the documented functions the ticket named (#1704); Bigtable SQL's geography `ST_` and `S2_` functions (#1720).

| Gap | Why open | Size |
|---|---|---|
| Whether production reorders an array's excluded values after its indexed ones, as the emulator does (#941) | needs live GCP | S |
| Firestore: `ExecutePipeline` runs on Enterprise edition databases since #1500, its stages and functions since #1523 and #1613 ([compatibility.md](compatibility.md)); left: full-text search (`document_matches`, `score`) is `UNIMPLEMENTED`, Google's reference defining neither its tokenization nor its score, and the explain stats' text is CloudBurrow's own (#1577) | not documented by Google | M |
| Spanner: 1 RPC refused, naming why: `InternalUpdateGraphOperation`, which Google documents as "an internal API called by Spanner Graph jobs. You should never need to call this API directly", every field "Internal field, do not use directly", so there is no behaviour to serve ([spanner.md](coverage/spanner.md)). `AddSplitPoints` is a stored no-op of the call front since #1523 (the emulator never splits), and `FetchCacheUpdate` answers an empty location-aware routing cache since #1523 (one server, nothing to route). Backups and restores, backup schedules, instance configurations (Google's and custom), `MoveInstance`, partition updates, database roles, the operation listings and IAM on backups and roles are served by the call front since #1498 (#1577) | not documented by Google | S |
| Bigtable: `OpenMaterializedView`'s sessions open but their `ReadRow` is refused `UNIMPLEMENTED`, as `ReadRows` of a materialized view is: Google documents reading a continuous materialized view with `ReadRows`, but neither the column family its aggregate columns are stored in nor how its row key is encoded without a `_key` column (docs re-read 2026-11-17: <https://docs.cloud.google.com/bigtable/docs/continuous-materialized-view-queries>), so its rows are read with SQL and the client falls back to the classic path ([bigtable.md](coverage/bigtable.md)). Change streams (since #1523) have one partition, which the API allows, and a collected cell's `GARBAGE_COLLECTION` record names the cell's millisecond as its time range, UNVERIFIED (docs re-read 2026-11-17: <https://docs.cloud.google.com/bigtable/docs/change-streams-overview> names none) (#1577) | not documented by Google | M |
| Bigtable: SQL runs GoogleSQL for Bigtable's `SELECT` with views (#1499, #1511), and since #1704 `UNPACK` and `_timestamp`, window functions, `STRUCT` constructors, the statistical and approximate aggregates, `FORMAT`, `SOUNDEX`, the vector, date, array (with the lambda functions `ARRAY_FILTER`, `ARRAY_TRANSFORM`, `ARRAY_OFFSET`, `ARRAY_OFFSETS`) and JSON functions, `NORMALIZE` and `NORMALIZE_AND_CASEFOLD` and an aggregate family's typed values, and since #1720 the geography `ST_` and `S2_` functions, on S2 (github.com/golang/geo), with since #1728 `ST_UNION`, `ST_UNION_AGG`, `ST_INTERSECTION`, `ST_DIFFERENCE`, `ST_BUFFER`, `ST_BUFFERWITHTOLERANCE` and `ST_SIMPLIFY` on internal/s2ops (UNVERIFIED where boundaries only touch, see [compatibility.md](compatibility.md)); left, refused `UNIMPLEMENTED` naming the reason: `ST_REGIONSTATS`, which reads Earth Engine rasters (#1728). `GROUPING`, `MIN_BY` and `MAX_BY` are refused saying GoogleSQL for Bigtable does not list them (#1704) | no emulator support | L |

## Cloud SQL, Memorystore

Closed: instance reads, databases, users and operations (#1301), instance writes, backups,
restores and clones, read replicas, point-in-time recovery and the connector path (#1330), imports
and exports, client certificates and server CA rotation, `tiers.list`, `flags.list` and
`operations.cancel` (#1380), database versions, major version upgrades, cascading and cross-region
replicas (#1381), high availability (#1408), Google's Auth Proxy binary (#1421),
`pointInTimeRestore` and Backup and DR backups through the Backup and DR Service (#1425, #1433);
the Memorystore admin API over Valkey (#1302), exports, imports and RDB persistence (#1384),
STANDARD_HA and failover (#1385), versions, upgrades and maintenance (#1386), in-transit
encryption (#1413), and customer-managed keys and server CA rotation (#1463); Memorystore for Redis Cluster's and Valkey's IAM
auth checks the access token against the tokens this instance minted and the project's connect role (#1614); a
`CLUSTER_DISABLED` Valkey instance's reader endpoint follows the replicas' health and its primary endpoint an
automatic failover, and a cross-instance secondary follows its primary instance's failover (#1614); Redis Cluster and Valkey
cluster clients on the host and in the Kubernetes cluster, each given addresses it reaches (#1614); Cloud SQL blue-green deployments
for MySQL (#1614). IAM database authentication is excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Memorystore for Redis: `REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` stay 501, as they are end of life with no maintained image (the Docker official `redis` image last built them in 2018, 2020 and 2022); with a customer-managed key, the RDB snapshots and AOF Valkey writes on a server's own volume are not encrypted with it, and are served only while the key can be used (#1463) (#1501: still no maintained image, so they stay 501) (#1524) | no emulator support: no maintained image; Valkey writes its own files | S |
| Backup and DR: workload types other than Cloud SQL instances (Compute Engine instances and disks, AlloyDB, Filestore), management servers and the appliance protocol: 14 methods 501, each naming what is missing in `tools/coverage/reasons.json`, as CloudBurrow runs no Compute Engine, AlloyDB, Filestore or backup appliance for them to act on (`encryptionConfig` with this instance's Cloud KMS and `resourceBackupConfigs.list` of Cloud SQL instances are built since #1501, `resourceBackupConfigs.fetch` with gcloud's filter, trials and `msComplianceMetadata` since #1536; [backupdr.md](coverage/backupdr.md)) (#1462) (#1501) (#1536) (#1524) | no emulator support (no Compute Engine, AlloyDB, Filestore or appliance) | L |
| Memorystore for Redis Cluster and for Valkey: cross-cluster (cross-instance) replication in CLUSTER mode and migration into CLUSTER mode are 501: a Valkey cluster-mode node refuses `REPLICAOF` and its replicas follow only primaries of their own cluster (measured #1563), and a keyspace scan would lose the writes made during it; migration from a source with AUTH is 501, `self_managed_source` having no field for its credentials (#1557) (#1563) (#1577) (#1519) | no emulator support (Valkey cluster mode refuses REPLICAOF) | M |
| Cloud SQL: what is left of the Admin API (a `PHYSICAL` external sync, served since #1570 with `--mode persistent`, is served on an ephemeral instance too since #1614, restored onto an `emptyDir` of the replica's pod by XtraBackup in ephemeral containers; a PostgreSQL primary's DR replica (`replicationCluster`) and `switchover`, which rebuilds the old primary from a `pg_basebackup` of the new one as its standby, are built since #1524; a MySQL external primary, its source representation instance, `verifyExternalSyncSettings`, `startExternalSync`, a PostgreSQL external primary, `dumpFilePath`, `OFFLINE` syncs, and `demoteMaster` and `demote` of a MySQL instance are built since #1501 and #1524, `demoteMaster` with `skipReplicationSetup`, which installs Cloud SQL's `mysql.*` replication stored procedures, `demote` of a PostgreSQL instance, and a switchover of a PostgreSQL primary with other read replicas, which stay its replicas, rebuilt from it, since #1614). 6 methods are 501, here by how often gcloud, Terraform and the connectors call them, each reason dated 2026-10-18 (#1614) and in [sqladmin.md](coverage/sqladmin.md): `rescheduleMaintenance` (`gcloud sql reschedule-maintenance`) needs a scheduled maintenance event, and none is ever scheduled here; the three Entra ID certificate methods and the two SSRS leases are SQL Server's, whose image is amd64 only. A `CUSTOMER_MANAGED_CAS_CA` instance is 501 (its CA pool is Certificate Authority Service's). `reencrypt`, a MySQL primary's DR replica and `instances.switchover` (`gcloud sql instances switchover`), `GOOGLE_MANAGED_CAS_CA` instances with `listServerCertificates`, `addServerCertificate` and `rotateServerCertificate` (`gcloud sql ssl server-certs`), `connect.resolve`, and the five `blueGreenDeployments` methods for MySQL (the switchover moves the blue instance's name and connection name to the green server; a `REGIONAL` blue instance's green instance is `REGIONAL`, its standby replicating from its primary; Google documents them for MySQL only) are built since #1614 (#1570) (#1614) (#1462) | not built yet | L |
| Backup and DR: `serviceConfig.initialize` is 501: it creates Google's default backup plan for a resource type, and the reference names none of its names, rules or retention (docs re-read 2026-10-17: https://docs.cloud.google.com/backup-disaster-recovery/docs/reference/rest/v1/projects.locations.serviceConfig/initialize); create them with `backupVaults.create` and `backupPlans.create` (#1536) (#1577) | not documented by Google | S |
| Cloud SQL: `MYSQL_5_6`, `MYSQL_5_7` and SQL Server versions, and `BAK` and `TDE` files, are 501: their images, `mysql:5.6`, `mysql:5.7` and `mcr.microsoft.com/mssql/server` 2022 and 2025, are linux/amd64 only (checked again with `docker manifest inspect` 2026-10-17); high availability, read replicas and point-in-time recovery on PostgreSQL 9.6 to 11 (no `pg_promote()` or `recovery.signal` in those versions) and upgrading a PostgreSQL replica on its own (streaming replication does not cross major versions) are 501 naming why (#1462) (#1501) (#1524) | no emulator support (amd64-only images; the engine versions lack the primitives) | M |

## Functions, credentials, tooling

Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319);
Cloud Functions v2 and v1 APIs (#1377, #1428); Pub/Sub and project IAM in Terraform (#1367,
#1368).

| Gap | Why open | Size |
|---|---|---|
| Terraform resources beyond the verified set (#1304), of the services served, with google 8.4.0: none unverified but `google_cloudfunctions_function_iam_policy` and `google_cloudfunctions2_function_iam_policy` (their tests build a function with Google's builder, `CLOUDBURROW_TEST_FUNCTIONS` on amd64, not run here); `google_developer_connect_connection`, `_git_repository_link` and `google_sql_provision_script` are verified since the last batch. Stated, not faked: creating a `google_developer_connect_connection` (8.4.0 has no `secure_source_manager_instance_config` block and its other configs authorize hosted providers, refused 501; an imported one is managed), a worker pool's `scaling_mode` (Google publishes no such field), `google_logging_folder_settings` (refused, as Google documents), `google_organization_iam_policy` (authoritative over the shared organization), `google_sql_source_representation_instance` (refused 501), log bucket CMEK (refused); `google_backup_dr_management_server` and `_restore_workload` (management servers and Compute Engine restores refused 501), `google_backup_dr_service_config`, `google_artifact_registry_locations` (Artifact Registry's Locations is not served), `google_memorystore_acl_policy`, `google_memorystore_instance_desired_user_created_endpoints` and `google_redis_cluster_user_created_connections` (not served), `google_secure_source_manager_branch_rule` and `_hook`, `google_developer_connect_account_connector`, `google_firestore_user_creds` and `google_cloudbuildv2_*` (refused 501), `google_tags_location_tag_binding` (regional tag bindings are not served), `google_cloud_run_locations` (the provider sends it to `run.googleapis.com` whatever `cloud_run_custom_endpoint` says), `google_storage_object_signed_url` (signs with a service account key, which `cloudburrow terraform` does not configure); the resources of APIs not served (BigQuery Data Transfer, Reservation, Connection, Data Policy and Analytics Hub; IAM, service accounts and Workload Identity; Cloud Billing accounts; Storage Transfer, Insights, Batch Operations and intelligence findings; Pub/Sub Lite; Vertex AI beyond prediction; Compute Engine; Cloud Identity, Asset and Access Approval; OAuth2 userinfo) (#1578) | no emulator support | S |

## Console

Closed by the sweep: Test permissions on the Permissions tab (#1242), and the request-rate,
latency and resource charts (#1267); the Artifact Registry repository Permissions tab (#1439).

| Gap | Why open | Size |
|---|---|---|

## Excluded on purpose

Not gaps to close; each stays until its decision is amended.

- **Cloud Storage ACLs and object IAM** (`bucketAccessControls.*`, `objectAccessControls.*`,
  `defaultObjectAccessControls.*`, `objects.*IamPolicy`): 501 by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #504.
- **IAM Conditions** on the policies of services Google does not list as allowing them (Pub/Sub,
  Cloud Tasks, Artifact Registry, Secure Source Manager, Cloud Functions): refused INVALID_ARGUMENT,
  as Google refuses them; served on projects, folders and the organization since #1742, and on the
  services Google lists (Bigtable, BigQuery, KMS, Logging, Run, Secret Manager, Spanner, Storage
  buckets with uniform bucket-level access, tag keys and values) since #1745. **Audit configs** are refused
  too, except on projects, folders and the organization, where they are stored, not acted on (#1486).
- **IAM enforcement**: [ADR-0006](adr/0006-iam-policy-surface.md) stores policies, never enforces them,
  but for Memorystore's IAM auth, which checks a client's token and the project's connect role (amended by #1614).
- **IAM Credentials `signBlob`**: refused because a local signature makes signed URLs no Google
  service accepts ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Cloud SQL IAM database authentication** (`generateEphemeralCert` with an access token,
  `cloudsql.iam_authentication`, users of an IAM type): 501, as CloudBurrow has no identities
  ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Local AI and embeddings in per-PR CI**, and the default build's embeddings answering 501:
  maintainer decisions recorded in compatibility.md (§What CI does not run, §Local AI, #41).
