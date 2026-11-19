# Cloud Monitoring

CloudBurrow serves the Cloud Monitoring API v3 `MetricService` (`monitoring.googleapis.com`, #1939) in the CLI process. Google publishes no emulator, so it is built to Google's reference: metric descriptors, monitored resource descriptors, user-defined time series written with Google's write rules, and reads with filters, alignment and reduction. The instance's own measurements are built-in metrics: its log-based metrics and log entry counts, from the local Cloud Logging, and the API requests it serves.

```sh
cloudburrow up --services logging,monitoring
eval "$(cloudburrow env)"
```

The API is gRPC and JSON on one port, `--port-monitoring` (default `9033`). `env` exports `CLOUDBURROW_MONITORING_ENDPOINT`; pass it to the official client:

```go
c, err := monitoring.NewMetricClient(ctx,
	option.WithEndpoint(os.Getenv("CLOUDBURROW_MONITORING_ENDPOINT")),
	option.WithoutAuthentication(),
	option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

OpenTelemetry's Google Cloud exporter takes the same endpoint and options.

## What is served

- **Metric descriptors**: create, get, list and delete user-defined ones (`custom.googleapis.com/`, `external.googleapis.com/`, `workload.googleapis.com/`); list and get the built-in ones.
- **Monitored resource descriptors**: the types user-defined metrics may be written against, and the ones the built-in metrics and log entries name.
- **Writing points** (`CreateTimeSeries`): one point per series per request, up to 200 series, in order, at most one point every 5 seconds per series, end times from 25 hours back to 5 minutes ahead; a descriptor is auto-created for a type that has none.
- **Reading series** (`ListTimeSeries`): a filter naming one metric type, an interval, every aligner and reducer, `group_by_fields`, the FULL and HEADERS views.
- **Built-in metrics**:
  - `logging.googleapis.com/user/NAME` for each enabled log-based metric (counter or distribution, one-minute DELTA points, labels from its extractors), counting the entries received after the metric was created;
  - `logging.googleapis.com/log_entry_count`, per log and severity;
  - `serviceruntime.googleapis.com/api/request_count` and `api/request_latencies` on `consumed_api`, for the APIs CloudBurrow serves itself, attributed to the instance's default project.

User-defined descriptors and points are kept in `monitoring.json` in the instance directory in persistent mode. Log-based values come from Cloud Logging's in-memory entries, and request metrics are kept in memory for a week.

Terraform's `google_monitoring_metric_descriptor` works through `cloudburrow terraform` (`monitoring_custom_endpoint`). gcloud has no command for metric descriptors or time series.

## Console

Operations > Monitoring has **Metrics explorer** (`/monitoring/metrics-explorer`): select a resource type and a metric, filter by label, choose an aligner, alignment period, reducer and group-by labels and a time range, and see the series charted and listed. **Metrics management** (`/monitoring/metrics-management`) lists the descriptors, creates user-defined ones and deletes them.

## Not served

Alerting policies, notification channels, dashboards, groups, uptime checks, snoozes, services and SLOs, and the Query service (MQL and PromQL) answer `UNIMPLEMENTED`. See [compatibility.md](compatibility.md#cloud-monitoring--googlemonitoringv3) for every claim and its gaps.
