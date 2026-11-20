# Cloud Monitoring

CloudBurrow serves the Cloud Monitoring API v3 (`monitoring.googleapis.com`, #1939, #1964) and the Dashboards API v1 in the CLI process. Google publishes no emulator, so it is built to Google's reference: metric descriptors, monitored resource descriptors, user-defined time series written with Google's write rules, and reads with filters, alignment and reduction. The instance's own measurements are built-in metrics: its log-based metrics and log entry counts, from the local Cloud Logging, and the API requests it serves, per project.

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
  - `serviceruntime.googleapis.com/api/request_count` and `api/request_latencies` on `consumed_api`, for the APIs CloudBurrow serves itself, attributed to the project each call names (a call that names none to the instance's default project);
  - with Firestore (#2048), `firestore.googleapis.com/document/read_ops_count` (`type` LOOKUP or QUERY), `write_ops_count` (`op` CREATE or UPDATE) and `delete_ops_count` on `firestore.googleapis.com/Database`, the same counts as `document/read_count`, `write_count` and `delete_count` on `firestore_instance`, and `query_stat/per_query/result_counts` and `scanned_documents_counts`: the successful document operations and query executions the Firestore front saw, by minute, kept for a day. A write counts as CREATE when its document did not exist and UPDATE when it did; a delete of a missing document is not counted, as Google's DELETE_NOOP is not; the Listen and Write streams count as Google bills them (#2050): a listener reads each document added to or updated in its results (one read for a query with no results), and each write of a Write stream counts as a commit's; the web SDK's WebChannel is not counted.

User-defined descriptors and points are kept in `monitoring.json` in the instance directory in persistent mode. Log-based values come from Cloud Logging's in-memory entries, and request metrics are kept in memory for a week.

Terraform's `google_monitoring_metric_descriptor` works through `cloudburrow terraform` (`monitoring_custom_endpoint`). gcloud has no command for metric descriptors or time series.

## Console

Operations > Monitoring has **Metrics explorer** (`/monitoring/metrics-explorer`): select a resource type and a metric, filter by label, choose an aligner, alignment period, reducer and group-by labels and a time range, and see the series charted and listed. **Metrics management** (`/monitoring/metrics-management`) lists the descriptors, creates user-defined ones and deletes them.

## Alerting, uptime checks, groups, SLOs and dashboards (#1964)

The other v3 services and the Dashboards API v1 are on the same port (`cloud.google.com/go/monitoring/apiv3/v2` clients and `cloud.google.com/go/monitoring/dashboard/apiv1`, with the same options):

- **Alert policies** with metric-threshold, metric-absence and log-match conditions, evaluated every 15 seconds over the series above and the local Cloud Logging. Each series that meets a condition opens an **incident**, which closes when it stops meeting it. Google has no incident API; the console's Alerting and Incidents pages list them, and acknowledge and close them. MQL and PromQL conditions are evaluated too (below). A `notificationChannelStrategy` with a `renotifyInterval` (30 minutes to 24 hours) repeats an open incident's notification to its channels.
- **Notification channels**. Delivery never leaves the machine. Webhook channels (`webhook_tokenauth`, `webhook_basicauth`) are posted Google's incident JSON (schema 1.2) only when their host is local: localhost, a loopback or private address, a cluster service name or `host.docker.internal`. A `pubsub` channel publishes to the instance's own Pub/Sub topic (with `--services pubsub`). Email, SMS, Slack, PagerDuty and Google Chat notifications are recorded and never sent; so are verification codes, which a channel's Deliveries on the console show. `VerifyNotificationChannel` takes that code; `GetNotificationChannelVerificationCode` of a verified channel gives one that verifies another channel with the same type and labels.
- **Snoozes**: while one is active, its policies open no incidents.
- **Uptime checks**: HTTP(S) and TCP checks of local hosts only, run once per period from one local checker. Each run writes `monitoring.googleapis.com/uptime_check/check_passed` and `request_latency`, which policies can alert on.
- **Groups**: their members are the monitored resources of the project's series.
- **Services and SLOs**: `ListTimeSeries` with `select_slo_health`, `select_slo_compliance`, `select_slo_budget`, `select_slo_budget_fraction`, `select_slo_burn_rate("SLO", "3600s")` and `select_slo_counts` computes them from the SLO's request-based or windows-based SLI over the local series. `basicSli` is refused.
- **Dashboards** are stored with an etag, and drawn on the console's Dashboards page.

Terraform's `google_monitoring_notification_channel`, `_alert_policy`, `_uptime_check_config`, `_group` and `_dashboard` work through `cloudburrow terraform`. `cloudburrow gcloud-setup` writes the `monitoring` override, so `gcloud monitoring dashboards`, `gcloud alpha|beta monitoring channels` and `policies` reach the instance.

Operations > Monitoring adds **Alerting**, **Incidents**, **Notification channels**, **Uptime checks**, **Dashboards**, **Groups**, **Services** and **Snoozes** pages.

## PromQL and MQL (#1966)

PromQL runs on the Prometheus engine over the local series, named as Google names them: `{"custom.googleapis.com/app/latency"}` or `custom_googleapis_com:app_latency`, `kubernetes_io:container_cpu_limit_cores`, with the metric and resource labels and `monitored_resource`. The Prometheus HTTP API Google serves is on the Monitoring port, so Grafana or `client_golang` can point at it:

```
http://localhost:9033/v1/projects/PROJECT/location/global/prometheus/api/v1/query?query=sum(custom_googleapis_com:app_latency)
```

`query`, `query_range`, `labels`, `label/NAME/values`, `series` and `metadata` are served. Alert policies take `conditionPrometheusQueryLanguage` (each series the query returns is true; it fires once true for `duration`).

MQL is deprecated by Google but still runs through the API, so `QueryService.QueryTimeSeries` and `conditionMonitoringQueryLanguage` are served for the core of the language: `fetch`, `metric`, `filter`, `align`, `every`, `within`, `group_by` and `condition val() > N`. Other table operations answer `UNIMPLEMENTED`.

The console offers them too ([#1991](https://github.com/cloudburrow/cloudburrow/issues/1991)): Metrics explorer's **PromQL** tab charts a range query; Create policy's **PromQL** condition takes a query, an evaluation interval and a retest window, and **Repeat notifications** sets `renotifyInterval` for the policy's channels (also on a policy's page); a service's page shows each SLO's status, compliance, error budget remaining and 1-hour burn rate, with Service level indicator and Error budget charts. MQL has no console page, as at Google.

## Not served

SQL conditions, forecast options and `basicSli` SLOs answer `UNIMPLEMENTED`. See [compatibility.md](compatibility.md#cloud-monitoring--googlemonitoringv3) for every claim and its gaps.
