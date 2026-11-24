# BigQuery Data Transfer Service

CloudBurrow serves `bigquerydatatransfer.googleapis.com` v1 (#1948) for **scheduled queries**
(`scheduled_query`) and **dataset copies** (`cross_region_copy`), whenever `bigquery` is
enabled. It runs in the CLI process on `--port-datatransfer` (default `9034`), gRPC and JSON on
one port, and is exported as `CLOUDBURROW_DATATRANSFER_ENDPOINT`. Nothing reaches Google.

```go
c, _ := datatransfer.NewClient(ctx, option.WithEndpoint(os.Getenv("CLOUDBURROW_DATATRANSFER_ENDPOINT")),
	option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

Terraform's `google_bigquery_data_transfer_config` works through `cloudburrow terraform`, which
sets `bigquery_data_transfer_custom_endpoint`.

## Runs

- A **scheduled query** runs as a BigQuery query job on the instance's BigQuery, with `@run_time`
  (TIMESTAMP) and `@run_date` (DATE) when the query names them. With a destination dataset, the
  results go to `destination_table_name_template`, rendered as Google documents (`{run_date}`,
  `{run_time|"%Y%m%d"}`, `{run_time+25h|"%Y%m%d"}`, `\{` and `\}`), with `write_disposition`
  `WRITE_TRUNCATE` or `WRITE_APPEND` (the default) and an optional `partitioning_field`.
- A **dataset copy** copies every table of `source_project_id`.`source_dataset_id` into the
  destination dataset; views, routines and external tables are skipped. With
  `overwrite_destination_table` the destination tables are replaced; without it, an existing
  table is left as it is.
- Runs go PENDING, RUNNING, then SUCCEEDED or FAILED (with `error_status`); one run per config at a
  time. Each has logs (`ListTransferLogs`).
- `StartManualTransferRuns` takes a `requested_run_time` (not in the future) or a
  `requested_time_range` (a backfill: a run per scheduled time, start included, end excluded);
  `ScheduleTransferRuns` is the deprecated backfill, both ends included.

## Schedules and the clock

Schedules use BigQuery's syntax, in UTC: `every 24 hours` (the default), `every 15 minutes`,
`every 2 hours synchronized`, `every 30 minutes from 08:00 to 18:00`, `every hour on mon,tue`,
`every day 00:00`, `every mon,wed 09:03`, `1st sunday of sep,oct,nov 00:00`,
`last friday of month 18:00`, `1,15 of month 09:00`. Scheduled queries run at most every 5
minutes and dataset copies every 12 hours, as Google documents. `schedule_options` gives a
`start_time`, an `end_time` and `disable_auto_scheduling`.

The service's clock is **BigQuery's**: the CloudBurrow extension
`POST /cloudburrow/bigquery/v2/clock:advance` with `{"seconds": n}` on the BigQuery endpoint moves
it, and the schedules that fall due run within a second.

## Notifications

- `notification_pubsub_topic`: each finished run is published to that topic on the instance's
  Pub/Sub, as the TransferRun in JSON with `eventType=TRANSFER_RUN_FINISHED` and
  `payloadFormat=JSON_API_V1`. A publish that fails is logged on the run as a warning.
- `email_preferences.enable_failure_email`: a failed run's email is recorded on the transfer's
  console page. No email is sent.

## Console

BigQuery > **Data transfers** lists every transfer config, and **Scheduled queries** those of
`scheduled_query`. Create transfer, a transfer's Run history and Configuration, Run transfer now,
Disable, Enable and Delete call the service's own methods.

## Not served

Every other data source (Google Ads, Cloud Storage, Amazon S3, Teradata and the rest) is refused;
`schedule_options_v2`'s `time_based_schedule` and `manual_schedule` (what bq sends) are carried into
`schedule` and `schedule_options`; its `event_driven_schedule`, for Cloud Storage transfers, is 501. Resource names carry the project number, as Google's do.

## bq

`bq mk|ls|show --transfer_config` work with `--api <BigQuery endpoint>`: bq sends Data Transfer calls
to the address `--api` names, and BigQuery's tunnel answers Data Transfer's discovery document
(`$discovery/rest?version=v1`) and passes `/v1/` to this service.
