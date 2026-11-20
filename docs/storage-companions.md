# Cloud Storage companion APIs

CloudBurrow serves three APIs that work on the instance's Cloud Storage buckets (#1957). They run in
the CLI process whenever `storage` is enabled, each on its own port, JSON only:

| API | Port flag | Default | `env` exports |
|---|---|---|---|
| Storage Batch Operations v1 (`storagebatchoperations.googleapis.com`) | `--port-storagebatchoperations` | `9035` | `CLOUDBURROW_STORAGEBATCHOPERATIONS_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGEBATCHOPERATIONS` |
| Storage Insights v1, inventory reports (`storageinsights.googleapis.com`) | `--port-storageinsights` | `9036` | `CLOUDBURROW_STORAGEINSIGHTS_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGEINSIGHTS` |
| Storage Transfer Service v1 (`storagetransfer.googleapis.com`) | `--port-storagetransfer` | `9037` | `CLOUDBURROW_STORAGETRANSFER_ENDPOINT`, `CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGETRANSFER` |

Use the official clients' REST transports with the endpoint and no credentials:

```go
c, err := storagetransfer.NewRESTClient(ctx,
	option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_STORAGETRANSFER_ENDPOINT")),
	option.WithoutAuthentication())
```

`cloudburrow terraform` sets `storage_transfer_custom_endpoint` and `storage_insights_custom_endpoint`,
so `google_storage_transfer_job` and `google_storage_insights_report_config` apply locally.

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
The console's Storage Transfer Service and Inventory reports pages offer it as Advance clock.

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
100 configurations. `reportDetails` records each report. Dataset configurations, which are BigQuery
linked datasets, are not served and answer 501.

## Storage Transfer Service

Transfer jobs copy between the instance's buckets, and between a bucket and a local directory. A local
directory is reached through an agent pool. CloudBurrow's own local agent serves every pool, so you do
not install Google's agents. A pool's bandwidth limit paces the transfers that go through it.

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
