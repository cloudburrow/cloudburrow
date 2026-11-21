# Cloud Trace

CloudBurrow serves Cloud Trace locally (#2118): the Cloud Trace API v1 and
v2 (`cloudtrace.googleapis.com`) and the Telemetry API's OTLP trace
ingestion (`telemetry.googleapis.com`), in the CLI process, on one port.
It is opt-in:

```sh
cloudburrow up --services storage,logging,trace
eval "$(cloudburrow env)"
```

`env` exports:

| Variable | What reads it |
|---|---|
| `CLOUDBURROW_TRACE_ENDPOINT` | the Cloud Trace Go clients (`cloud.google.com/go/trace/apiv1` and `apiv2`) and Google's OpenTelemetry exporter: pass it to `option.WithEndpoint`, with no credentials and plaintext gRPC |
| `CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDTRACE` | gcloud |
| `CLOUDBURROW_TELEMETRY_ENDPOINT` | OpenTelemetry's OTLP exporters: set `OTEL_EXPORTER_OTLP_ENDPOINT` to it (OTLP/gRPC at the root, OTLP/HTTP at `/v1/traces`), with `OTEL_EXPORTER_OTLP_INSECURE=true` for gRPC |

## Writing spans

- **OTLP**, as Google's guide to the OTLP endpoint configures it: the root
  URL, and `OTEL_RESOURCE_ATTRIBUTES="gcp.project_id=PROJECT_ID"` naming the
  project. Without `gcp.project_id` the request's `x-goog-user-project` is
  the project, and without that the instance's default project.
  `http/protobuf`, gRPC and OTLP/JSON are taken.
- **Cloud Trace API v2**: `BatchWriteSpans` and `CreateSpan`, with the
  reference's rules and limits.
- **Cloud Trace API v1**: `PatchTraces`, which merges into existing spans.
- **Google's exporter** (`opentelemetry-operations-go/exporter/trace`):
  `texporter.WithTraceClientOptions` with the endpoint options above.

## Reading traces

`ListTraces` takes the documented filter language
(<https://docs.cloud.google.com/trace/docs/trace-filters>), views, interval
and ordering; `GetTrace` reads one trace. A span from any writer reads the
same: its attributes are its v1 labels.

The console's Trace Explorer searches the projects of the chosen trace
scope (Observability API trace scopes, #2108). It draws the latency
heatmap (a cell lists its spans) and the Chart view's Span duration
(percentile) and Span rate, and lists the spans in the Spans table or
grouped by service and span name in the Grouped table. A trace's details
show its timeline or its Graph, Find in Trace, and the log entries whose
`trace` and `spanId` name its spans, from the instance's Cloud Logging.

## Limits

Spans are kept 30 days, as Google keeps them. See
[compatibility.md](compatibility.md#cloud-trace--googledevtoolscloudtracev1-and-v2)
for what is verified, and what Google leaves undocumented.
