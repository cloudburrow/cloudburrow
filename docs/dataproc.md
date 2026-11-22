# Serverless for Apache Spark (Dataproc batches and sessions)

CloudBurrow serves the serverless surface of the Dataproc API
(`dataproc.googleapis.com` v1), which Google's console calls Serverless for
Apache Spark (the product is being renamed Managed Service for Apache Spark):
batch workloads, interactive Spark Connect sessions and session templates
(#2156). Clusters, cluster jobs, workflow templates, autoscaling policies and
node groups are not served.

It is opt-in, in the CLI process, beside BigQuery:

```sh
cloudburrow up --services bigquery,storage,logging,dataproc
```

`--port-dataproc` (default `9066`) serves gRPC and JSON on one port.
`cloudburrow env` exports `CLOUDBURROW_DATAPROC_ENDPOINT`. No client
library reads it: pass it to `option.WithEndpoint` with
`option.WithoutAuthentication()`.

```go
c, err := dataproc.NewBatchControllerClient(ctx, option.WithEndpoint(os.Getenv("CLOUDBURROW_DATAPROC_ENDPOINT")),
	option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

## Where workloads run

Batches and sessions run on the Spark runner of BigQuery's stored procedures
for Apache Spark (`internal/sparkproc`, [architecture.md](architecture.md)). Each
one is `spark-submit --master local[*]` in the pinned `apache/spark` image of
its runtime version, with Google's Spark BigQuery connector reading and
writing the instance's BigQuery (its Storage Read and Write APIs). It runs on
a `docker network create --internal` network whose one peer relays the
instance's BigQuery ports, so a workload reaches nothing else. The images are
pulled on the first run (about 1.5 GB each).

| Runtime version | Spark | Notes |
|---|---|---|
| `1.2` | 3.5 | |
| `2.2` (default) | 3.5 | The default, as Google's runtime page has it |
| `2.3` | 3.5 | |
| `3.0` | 4.0 | Spark Connect sessions need it or `4.0` |
| `4.0` | 4.0 | |

A version Google does not list is `INVALID_ARGUMENT`.

## Batches

- `CreateBatch`: PySpark (`pyspark_batch`), Spark (`spark_batch`, a main class
  or main jar), SparkR (`spark_r_batch`) and Spark SQL (`spark_sql_batch`, its
  `query_variables` applied as `SET name="value";` before the script).
  - Main files, Python files, jars, files and archives are read from the
    instance's Cloud Storage (`gs://`). A `file://` path is one in the runtime
    image: `file:///usr/lib/spark/examples/jars/spark-examples.jar` runs
    SparkPi, as Google's quickstart does.
  - `batch_id` is 4 to 63 lower-case letters, digits and hyphens. Without
    one, a UUID-based ID is generated.
  - `request_id` makes a create idempotent.
  - `runtime_config.properties` go to `spark-submit --conf`.
  - `execution_config.service_account` runs the batch as that account: the
    connector's token is minted for it. `kms_key` is checked as for a stored
    procedure.
- The operation is `projects/P/regions/R/operations/ID`. Its metadata is a
  `BatchOperationMetadata`, and when the batch ends its response is the
  `Batch`, or its error the failure.
- State: `PENDING`, then `RUNNING`, then `SUCCEEDED`, `FAILED` or
  `CANCELLED`, each recorded in `state_history`.
  - `runtime_info.output_uri` is the driver output in the staging bucket.
  - `runtime_info.approximate_usage` comes from the executors and cores
    properties.
- `GetBatch`, `ListBatches` and `DeleteBatch`.
  - `ListBatches` takes a `filter` on `batch_id`, `batch_uuid`, `state`,
    `create_time` and `labels`, and an `order_by` of `create_time desc`.
  - Deleting a batch that has not ended is `FAILED_PRECONDITION`.
- Cancel: `operations.cancel` on the batch's operation, as Google documents.
  The batch goes to `CANCELLING`, then `CANCELLED`.
- Logs: the driver's lines are in Cloud Logging under resource type
  `cloud_dataproc_batch` (labels `project_id`, `location`, `batch_id`):
  - the driver's output in `dataproc.googleapis.com/output`;
  - Spark's own log4j lines in `dataproc.googleapis.com/spark`, with a
    `component` label.
- Staging bucket: `dataproc-staging-REGION-PROJECT_NUMBER-SUFFIX`, made when
  first needed. The suffix is UNVERIFIED (docs re-read 2026-11-22:
  <https://docs.cloud.google.com/managed-spark/docs/concepts/staging-bucket-serverless>,
  which does not give it).

## Interactive sessions

- `CreateSession` with `spark_connect_session` starts a Spark Connect server
  (Spark's `SparkConnectServer`) in the runtime's image.
  - The session is `CREATING`, then `ACTIVE`.
  - `runtime_info.endpoints["Spark Connect Server"]` is `sc://HOST:PORT`, the
    address the caller reached the API on.
- A Spark Connect client reaches the session through the API's port: every
  Spark Connect request carries its `session_id`, which is the session's
  UUID. There are two ways in:
  - **gRPC on the port itself**, for a plain client:
    `SparkSession.builder.remote("sc://127.0.0.1:9066/;session_id=UUID")`;
  - **the TCP-over-WebSocket bridge**
    (`/tcp-over-websocket-bridge/35218cb7-1201-4940-89e8-48d8f03fed96`),
    which Google's `dataproc-spark-connect` client tunnels its HTTP/2
    connection through.
- `session_template` applies a template's settings where the session sets
  none.
- `execution_config.idle_ttl` (default 1 hour) and `ttl` (10 minutes to 14
  days) end the session.
- `TerminateSession`, `GetSession`, `ListSessions` (with a `filter`) and
  `DeleteSession`. Delete terminates the session first.
- `jupyter_session` is `UNIMPLEMENTED`: a Jupyter session's kernel is served
  through Google's Jupyter gateway, which no local instance has.

## Session templates

`CreateSessionTemplate`, `GetSessionTemplate`, `ListSessionTemplates`,
`UpdateSessionTemplate` (a full replacement) and `DeleteSessionTemplate`. The
template ID starts and ends with a letter or digit.

## Console

Dataproc's Serverless menu:

- **Batches**: every region's batches.
  - **Create batch** is a page of its own: the batch type's main file,
    arguments, files, runtime version, service account, properties and
    labels.
  - A batch's page has Summary, State history, Output (the driver output,
    read from Cloud Storage) and Job logs (the Cloud Logging filter),
    with Cancel and Delete.
- **Interactive sessions**:
  - Create from a template or a runtime version;
  - a session's page shows its Spark Connect endpoint and a PySpark line
    to connect with;
  - Terminate and Delete.
- **Session templates**: Create, Edit and Delete.

BigQuery Studio's **Apache Spark notebook** (Home, Create new) opens a
notebook whose first cell is Google's:

```python
from google.cloud.dataproc_spark_connect import DataprocSparkSession
from google.cloud.dataproc_v1 import Session

spark = DataprocSparkSession.builder.dataprocSessionConfig(Session()).getOrCreate()
```

The notebook runtime (`internal/nbruntime`) has Google's
`dataproc-spark-connect` 1.1.0 and `google-cloud-dataproc` 5.32.0, with
`pyspark-client` 4.0.1. Its relay forwards the Dataproc port too (`:8081` on
the runtime's internal network). The kernel points the Dataproc clients and
the WebSocket bridge at it, with local credentials, as it does for BigQuery.

## What is not served

- Jupyter sessions, as above.
- `pyspark_notebook_batch`, which is `UNIMPLEMENTED`.
- Network settings (`subnetwork_uri`, `network_tags`), which are accepted
  and kept but have no effect: a local run has no VPC.
- Autoscaling, which is not run. The executors are `local[*]` threads of
  the driver's container.
- The Spark UI and persistent history server (`peripherals_config`).
- SparkR: the `apache/spark` images carry no R, so an R batch fails with
  the runner's error.
- Clusters and the rest of the Dataproc API: not served.
