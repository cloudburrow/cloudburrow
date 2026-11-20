# Cloud Storage companion APIs

CloudBurrow serves three APIs that work on the instance's Cloud Storage buckets (#1957). They run in
the CLI process whenever `storage` is enabled, each on its own port, JSON only:

| API | Port flag | Default | `env` exports |
|---|---|---|---|
| Storage Batch Operations v1 (`storagebatchoperations.googleapis.com`) | `--port-storagebatchoperations` | `9035` | `CLOUDBURROW_STORAGEBATCHOPERATIONS_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGEBATCHOPERATIONS` |
| Storage Insights v1, inventory reports and datasets (`storageinsights.googleapis.com`) | `--port-storageinsights` | `9036` | `CLOUDBURROW_STORAGEINSIGHTS_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGEINSIGHTS` |
| Storage Transfer Service v1 (`storagetransfer.googleapis.com`) | `--port-storagetransfer` | `9037` | `CLOUDBURROW_STORAGETRANSFER_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_TRANSFER` (gcloud's property is `transfer`) |

Use the official clients' REST transports with the endpoint and no credentials:

```go
c, err := storagetransfer.NewRESTClient(ctx,
	option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_STORAGETRANSFER_ENDPOINT")),
	option.WithoutAuthentication())
```

`cloudburrow terraform` sets `storage_transfer_custom_endpoint` and `storage_insights_custom_endpoint`,
so `google_storage_transfer_job`, `google_storage_insights_report_config` and
`google_storage_insights_dataset_config` apply locally.

Every object is read and written through the instance's Cloud Storage JSON API, so versioning, holds,
retention, soft delete and CMEK behave as they do for any other caller. Records are kept under the
instance's state directory (`storagejobs/`).

## The clock

Transfer schedules and inventory report frequencies follow one clock shared by the three APIs. It
starts at this machine's time and only moves forward, through a CloudBurrow extension on any of the
three ports:

```sh
curl -X POST http://$CLOUDBURROW_STORAGETRANSFER_ENDPOINT/cloudburrow/v1/clock:advance -d '{"duration":"86400s"}'
```

The call runs every transfer and writes every report that the new time makes due before it answers.
The console's Storage Transfer Service, Inventory reports and Insights datasets pages offer it as
Advance clock.

## Storage Batch Operations

A job runs as soon as it is created, object by object, and its long-running operation is done when
the job ends. Google's rules are kept:

- the location is `global`;
- each job has one transformation: delete objects, put metadata, rewrite to a Cloud KMS key, put object
  holds, or update object contexts;
- objects are chosen by prefixes (at most 1,000) or by a CSV manifest of `bucket,name[,generation]`;
- a job names at most 1,000 buckets, and Requester Pays buckets are refused;
- the project needs Storage Intelligence, edition STANDARD or TRIAL (`gcloud storage
  intelligence-configs enable`), as Google requires each bucket to be enrolled;
- a dry run counts the objects and changes nothing;
- a job still running after 14 days is cancelled.

An object that fails is counted in `failedObjectCount` and summarized in `errorSummaries`. The job
still ends SUCCEEDED; it is FAILED only when it cannot run at all, such as when its manifest cannot be
read.

## Storage Insights inventory reports

A report configuration writes one report per occurrence of its frequency, DAILY or WEEKLY from
`startDate` at 00:00 UTC through `endDate`, once the clock reaches it. Each report lists the source
bucket's live objects with the chosen metadata fields, as one shard and a manifest, with Google's
names:

```
<destinationPath>/<config-id>_<YYYY-MM-DDTHH:MM>_0.csv     (or .parquet)
<destinationPath>/<config-id>_<YYYY-MM-DDTHH:MM>_manifest.json
```

The destination bucket must be in the source bucket's project and location, and a bucket takes at most
100 configurations. `reportDetails` records each report.

## Storage Insights datasets

A dataset configuration (`datasetConfigs`, #1976) takes a snapshot of its scope's project, bucket and
object metadata when it is made and every 24 hours after on the shared clock (Google takes the first
within 24 to 48 hours), and keeps the snapshots of the last `retentionPeriodDays` days (1 to 90;
`activityDataRetentionPeriodDays`, 0 to 365, is kept). Every change answers a long-running operation
that is already done; `operations` get, list, cancel and delete them.

- **Location**: one of Google's linked-dataset locations: `us`, `eu`, `asia-south1`, `asia-south2`,
  `asia-southeast1`, `europe-west1`, `us-central1`, `us-east1`, `us-east4`. Source buckets can be
  anywhere.
- **Name**: up to 128 letters, numbers and underscores, starting with a letter.
- **Scope**: `sourceProjects` (project numbers), `sourceFolders` (every project under them),
  `organizationScope` (with `organizationNumber`), or `cloudStorageObjectPath`, a `gs://` file of
  `projects/N` or `folders/N` lines (not both), which becomes the projects or folders it names. With
  `organizationNumber`, projects outside it are left out with an `error_attributes_view` row (code 1);
  a source project without Storage Intelligence is left out with code 4. At most 10,000 projects or
  folders.
- **Filters**: include or exclude bucket locations, and include or exclude buckets by `bucketName` or
  `bucketPrefixRegex` (RE2, matched from the start of the name). Without
  `includeNewlyCreatedBuckets`, only the buckets that existed when the configuration was made.
- **Identity**: `IDENTITY_TYPE_PER_CONFIG` (the default) or `IDENTITY_TYPE_PER_PROJECT`, whose service
  agent is `service-PROJECT_NUMBER@gcp-sa-storageinsights.iam.gserviceaccount.com`, as Google documents.
  The per-configuration agent's form is not documented; CloudBurrow names it
  `service-PROJECT_NUMBER-UIDPREFIX@gcp-sa-storageinsights.iam.gserviceaccount.com`.
- The configuration's project needs Storage Intelligence (STANDARD, TRIAL or ESSENTIALS).

`linkDataset` makes the linked dataset in the instance's BigQuery (`--services bigquery`), in the
configuration's project and location, named after the configuration's ID and its uid (as Google's
example, `my_dataset276daa7e_2991_...`, shows); `link.dataset` is `PROJECT.DATASET`. It holds Google's
views with Google's columns:

| View | Rows |
|---|---|
| `bucket_attributes_view`, `bucket_attributes_latest_snapshot_view` | each bucket, with `objectCount` and `totalSize` |
| `object_attributes_view`, `object_attributes_latest_snapshot_view` | each live object (no checksums for a Cloud KMS-encrypted object) |
| `project_attributes_view` | each project in scope |
| `object_contexts_catalog_view` | the latest snapshot's objects that have contexts |
| `events_view` | `eventCode` 1 per view refreshed and 2 per snapshot, and 3 per activity view refreshed |
| `error_attributes_view` | the projects and buckets left out, and why |
| `object_events_view` | each object write, update and delete, and each 429, since the previous snapshot, with the object's size, generation and storage class from this one (#1979) |
| `project_activity_view`, `bucket_activity_view` | per project and per bucket in scope, the requests since the previous snapshot (`snapshotStartTime` to `snapshotEndTime`) by kind and status, and bytes each way; a bucket's `topPrefixes*` records are its top 5 object prefixes (the name up to its last `/`), as `prefix` and `count` (Google names neither field: UNVERIFIED (docs re-read 2026-11-19: <https://docs.cloud.google.com/storage/docs/insights/dataset-tables-and-schemas>)) |
| `bucket_region_activity_view` | per bucket, its bytes each way; `requestLocation` is null, since every request comes from this machine, in no Google Cloud location |

The activity views are made from the instance's Cloud Storage request log, the record its Data
Access audit log entries come from (docs/compatibility.md, #1979): every request on a bucket in scope
counts, audit logging on or off, except the snapshot's own reads. Activity rows are kept for
`activityDataRetentionPeriodDays` (the dataset's retention when it is unset, as Google documents; 0
keeps none), also past the dataset's retention, up to 365 days. The requests between snapshots are
kept in memory (at most 200,000), so a restart of the CLI loses those not yet in a snapshot.

Each snapshot refreshes the dataset. It is read-only to every client: the views read tables that
`tables.list` does not show, and the BigQuery front refuses writes into the dataset (403
`accessDenied`). `unlinkDataset` deletes it; a configuration cannot be deleted while it is linked, as
Google documents, so with Terraform set `link_dataset = false` before `destroy`.

## Storage Transfer Service

Transfer jobs copy between the instance's buckets, and between a bucket and a local directory. A local
directory is reached through an agent pool. CloudBurrow's own local agent serves every pool, so you do
not install Google's agents. A pool's bandwidth limit (`limitMbps`, megabytes per second, as Google documents) paces the transfers that go through it.

Jobs support the following:

- object conditions;
- transfer options: `overwriteWhen`, overwriting objects already in the sink, deleting objects found
  only in the sink, and deleting from the source after transfer;
- transfer manifests;
- schedules;
- Pub/Sub notifications to the instance's Pub/Sub.

Runs are long-running transfer operations. Each has Google's counters, and it can be paused, resumed or
cancelled.

Some sources are refused with 501 because they are outside this machine: Amazon S3, S3-compatible
storage, Azure Blob Storage, HDFS and URL lists. CloudBurrow never reaches them. Event-driven
transfers and replication are not served yet.
