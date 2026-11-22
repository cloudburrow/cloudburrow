# Dataproc: clusters, jobs, workflows and Serverless for Apache Spark

CloudBurrow serves the Dataproc API (`dataproc.googleapis.com` v1):

- **Serverless for Apache Spark** (#2156; the product is being renamed
  Managed Service for Apache Spark): batch workloads, interactive Spark
  Connect and Jupyter sessions, and session templates;
- **Dataproc on Compute Engine** (#2162): clusters, jobs on them, workflow
  templates and autoscaling policies;
- the **Spark UI** of every workload, in Apache Spark's History Server.

Dataproc on GKE (virtual clusters) is not served.

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
- `jupyter_session` (#2162) runs the same Spark Connect server, with the
  Python kernel (`kernel` `PYTHON`, the default). Its kernel is a notebook
  of the console's notebook runtime (`internal/nbruntime`): the session's
  page gives the line that binds `spark` to the session there,
  `SparkSession.builder.remote("sc://host.docker.internal:8081/;session_id=UUID")`,
  and any Spark Connect client reaches it on the API's port by its UUID.
  Google documents no `runtime_info.endpoints` key for a Jupyter session's
  kernel gateway, so it has none. `kernel` `SCALA` is `UNIMPLEMENTED`: no
  Scala kernel runs locally.

## Notebook and SparkR batches

- `pyspark_notebook_batch` (#2162) runs a Jupyter notebook (`.ipynb`) from
  Cloud Storage as the batch's PySpark driver. Its code cells run in order
  in one Python process on the runtime; markdown and raw cells are skipped.
  - `params` are set as papermill sets them: a cell of `NAME = "VALUE"`
    lines after the cell tagged `parameters`, or first when no cell is
    tagged. A key that is not a Python identifier is `INVALID_ARGUMENT`.
  - `python_file_uris`, `jar_file_uris`, `file_uris` and `archive_uris` are
    as for a PySpark batch.
  - IPython magics and shell commands (`%` and `!` lines) have no IPython
    to run on in the Spark runtime's Python. A notebook with one fails,
    naming the cell.
  - Google does not say where an executed notebook is written. The driver
    output is the batch's `output_uri`, as for any batch.
- `spark_r_batch` runs in the official `apache/spark` image of the runtime's
  Spark with Python and R (`-python3-r-ubuntu`, pinned by digest in
  `dependencies.json`). R runs in that container only: nothing of R is in
  the CLI or the repository.

## Session templates

`CreateSessionTemplate`, `GetSessionTemplate`, `ListSessionTemplates`,
`UpdateSessionTemplate` (a full replacement) and `DeleteSessionTemplate`. The
template ID starts and ends with a letter or digit.

## Clusters

A Dataproc cluster (#2162) is an Apache Spark standalone cluster in
containers:

- a **master** container (`CLUSTER-m`) runs Spark's Master and History
  Server;
- each **worker** container (`CLUSTER-w-N`) runs a Spark Worker with its
  machine type's vCPUs and memory (4 GB a vCPU for `standard` types, 8 for
  `highmem`, 1 for `highcpu`; 2 cores and 2 GB otherwise);
- they run on a `docker network create --internal` network whose one peer
  relays the instance's BigQuery and Cloud Storage ports, as for a batch;
- the image is the official `apache/spark` image of the image version's
  Spark, with Python and R, and Google's BigQuery connector for that Spark.

| Image version | Spark | Image |
|---|---|---|
| `2.1-debian11`, `-ubuntu20`, `-rocky8` | 3.3 | `3.3.3-scala2.12-java11-python3-r-ubuntu` |
| `2.2-debian12`, `-ubuntu22`, `-rocky9` | 3.5 | `3.5.9-scala2.12-java17-python3-r-ubuntu` |
| `2.3-debian12`, `-ubuntu22`, `-rocky9` | 3.5 | as 2.2 |
| `3.0-debian13` (default), `-ubuntu24`, `-rocky9` | 4.1 | `4.1.2-scala2.13-java21-python3-r-ubuntu` |

The image version list gives each version's Spark (re-read 2026-11-22:
<https://docs.cloud.google.com/dataproc/docs/concepts/versioning/dataproc-version-clusters>).
The default is "the latest generally available Debian-based" image. 1.x and
2.0 are past their end of support, and are `INVALID_ARGUMENT`. The operating
system names the image version only: every container runs the Spark
image's Ubuntu.

- `CreateCluster` is a long-running operation. Its metadata is a
  `ClusterOperationMetadata` of type `CREATE`, and its response the
  `Cluster`, which goes from `CREATING` to `RUNNING`.
  - The name starts with a lower-case letter, has at most 51 lower-case
    letters, digits and hyphens, and does not end with a hyphen.
  - The cluster types are Standard (1 master and at least 2 workers, 2 by
    default), High Availability (3 masters) and Single Node
    (`dataproc:dataproc.allow.zero.workers=true` and no workers). A High
    Availability cluster's three masters are one Spark master here.
  - `config_bucket` and `temp_bucket` default to
    `dataproc-staging-REGION-NUMBER-SUFFIX` and
    `dataproc-temp-REGION-NUMBER-SUFFIX` in the instance's Cloud Storage.
  - The labels `goog-dataproc-cluster-name`, `-cluster-uuid` and `-location`
    are added, as Google adds them.
  - `software_config.properties` with the `spark:` prefix become the
    cluster's `spark-defaults.conf`.
  - `endpoint_config.enable_http_port_access` (the component gateway) gives
    `http_ports["Spark History Server"]`: the cluster's History Server, at
    `/component-gateway/UUID/sparkhistory/` on the API's port.
  - Optional components (#2162) run as containers of their projects'
    official images in the master's network, so they answer at
    `CLUSTER-m:PORT`: JUPYTER (JupyterLab built on the cluster's Spark
    image, with PySpark and Python 3 kernels), TRINO (`trinodb/trino`,
    catalogs `tpch`, `tpcds`, `memory` and `hive`), FLINK (a JobManager and
    a TaskManager), HIVE_WEBHCAT, ZOOKEEPER, SOLR and, on 2.3 and later,
    PIG. With the component gateway each web interface is in `http_ports`
    (`Jupyter`, `JupyterLab`, `Trino`, `Flink`, `Solr`, `WebHCat`). A
    component the image version does not offer, or one named twice, is
    `INVALID_ARGUMENT`. ZEPPELIN, DOCKER, RANGER, DELTA, ICEBERG and HUDI
    are `UNIMPLEMENTED`, each with its reason (docs/gaps.md).
  - Every cluster has a Hive metastore (Derby, in the cluster's files) and
    HiveServer2 on its master, started with its first Hive job or with a
    component that uses them.
  - `auxiliary_node_groups` of role DRIVER: driver node groups. Each node
    is a container of the cluster's image named `CLUSTER-np-XXXX`; a job's
    driver runs in a node's network. A job on such a cluster needs
    `driver_scheduling_config`, as Google's driver node groups page says.
    `NodeGroupController`'s `CreateNodeGroup`, `GetNodeGroup` and
    `ResizeNodeGroup` manage them. A cluster with node groups cannot be
    stopped or autoscaled.
  - Dataproc on GKE (`virtual_cluster_config`) is `UNIMPLEMENTED`.
- `GetCluster`, `ListClusters` and `DeleteCluster`. `ListClusters` takes a
  `filter` on `status.state`, `clusterName` and `labels.KEY`, joined by
  `AND`.
- `UpdateCluster` takes the update masks Google documents:
  - `labels`;
  - `config.worker_config.num_instances`, which resizes the cluster by
    starting or stopping workers, and
    `config.secondary_worker_config.num_instances`;
  - `config.autoscaling_config.policy_uri`;
  - the lifecycle fields: `config.lifecycle_config.idle_delete_ttl`,
    `auto_delete_ttl` and `auto_delete_time`, and their stop counterparts.
- `StopCluster` and `StartCluster` stop the containers and start them
  again. The cluster's files are kept.
- `DiagnoseCluster` writes a tarball to the staging bucket and returns its
  `output_uri`. The tarball holds `summary.txt`, each node's Spark log and
  `spark-defaults.conf`.
- Scheduled deletion and stop: a cluster idle (no job running) for
  `idle_delete_ttl`, or older than `auto_delete_ttl`, is deleted. The stop
  fields stop it instead.
- In a persistent instance, clusters are kept and started again with the
  instance.

## Jobs

- `SubmitJob` and `SubmitJobAsOperation` run a job on the cluster its
  `placement` names, by `cluster_name` or `cluster_labels`. The driver is a
  container in client mode against the cluster's master.
  - PySpark, Spark, SparkSql (a query file or a query list, with
    `script_variables`) and SparkR jobs run.
  - Hadoop, Hive and Pig jobs run in the official `apache/hive` image
    (Hadoop 3.3.6, Hive 4.0.1, Tez) with Google's Cloud Storage connector
    for Hadoop, `gs://` reaching the instance's Cloud Storage through the
    cluster's relay. Hadoop MapReduce uses Hadoop's local job runner in
    the driver's container (there is no YARN); Hive jobs run Beeline
    against the cluster's HiveServer2; Pig 0.18.0 runs in local mode.
  - Trino jobs run the Trino CLI against the TRINO component, and Flink
    jobs `flink run` against the FLINK component; without the component
    they are `FAILED_PRECONDITION`. Presto is not available on image
    version 2.1 and later.
  - A job on a cluster that is not `RUNNING` is `FAILED_PRECONDITION`.
- State: `PENDING`, `SETUP_DONE` and `RUNNING`, then `DONE`, `ERROR` or
  `CANCELLED`, each recorded in `status_history`.
  `scheduling.max_failures_per_hour` restarts a failed driver
  (`ATTEMPT_FAILURE`).
- Driver output: `driver_output_resource_uri` is
  `gs://CONFIG_BUCKET/google-cloud-dataproc-metainfo/CLUSTER_UUID/jobs/JOB_ID/driveroutput`,
  written as `driveroutput.000000000`, where gcloud reads it.
- `yarn_applications` holds the Spark application. Its `tracking_url` is the
  application's page in the cluster's History Server.
- `GetJob`, `ListJobs`, `UpdateJob` (`labels`), `CancelJob` and
  `DeleteJob`. `ListJobs` takes `cluster_name`, `job_state_matcher`, and a
  `filter` on `status.state` and `labels.KEY`. Deleting a job that has not
  ended is `FAILED_PRECONDITION`.
- With `dataproc:dataproc.logging.stackdriver.job.driver.enable=true`, the
  driver's output is in Cloud Logging too, under resource
  `cloud_dataproc_job`, log `dataproc.job.driver`.

## Workflow templates

- `CreateWorkflowTemplate`, `GetWorkflowTemplate` (with `version`),
  `ListWorkflowTemplates`, `UpdateWorkflowTemplate` and
  `DeleteWorkflowTemplate`. An update must carry the current `version`.
  - Step IDs are unique. Prerequisites must name steps and form no cycle.
  - `parameters` name fields of the template, such as
    `jobs['STEP'].pysparkJob.args[0]`, and validate their values by regex
    or by a list of values.
  - `dag_timeout` is 10 minutes to 24 hours.
  - A stored template may be created without a placement or jobs, as
    gcloud's `workflow-templates create` makes one before
    `set-managed-cluster` and `add-job`. Instantiating it then is
    `INVALID_ARGUMENT`. An inline template needs both.
- `InstantiateWorkflowTemplate` and `InstantiateInlineWorkflowTemplate` run
  the workflow. The operation's `WorkflowMetadata` records its graph.
  - A **managed cluster** is created as `PREFIX-SUFFIX`, runs the jobs, and
    is deleted after.
  - A **cluster selector** runs them on a running cluster with those
    labels.
  - Each step's job ID is `STEP-SUFFIX`, labelled
    `goog-dataproc-workflow-template-id`, `-instance-id` and `-step-id`.
  - A failed job, a cancel (`operations.cancel`) or the DAG timeout ends the
    workflow and cancels the jobs still running.

## Autoscaling policies

- `CreateAutoscalingPolicy`, `GetAutoscalingPolicy`,
  `ListAutoscalingPolicies`, `UpdateAutoscalingPolicy` and
  `DeleteAutoscalingPolicy`. A policy a cluster uses cannot be deleted. The
  bounds are Google's: factors from 0.0 to 1.0, a graceful decommission
  timeout of at most 1 day, a cooldown of 2 minutes to 1 day.
- A cluster with `autoscaling_config.policy_uri` is scaled by its policy.
  After each cooldown, the scaler reads the master's pending and available
  cores and resizes the workers within the policy's bounds. A Spark
  standalone cluster has no YARN memory metrics, so the factors apply to
  cores.

## The Spark UI and the History Server

- Every workload's Spark event log is kept.
- A cluster's History Server runs on its master, at its component gateway
  path.
- The instance's own History Server shows the batches' and sessions'. It
  starts when first opened, at `/sparkhistory/` on the API's port. A
  batch's or session's **View Spark UI** opens
  `/sparkhistory/history/app-UUID/jobs/` there.
- A **Persistent History Server** is a cluster whose
  `spark:spark.history.fs.logDirectory` is a `gs://` path, in which a `*`
  matches one path segment. Its History Server reads the event logs there.
  - A cluster's jobs write theirs to `spark:spark.eventLog.dir`, or else to
    the temp bucket's `CLUSTER_UUID/spark-job-history`.
  - A batch's or session's
    `peripherals_config.spark_history_server_config.dataproc_cluster`
    copies its log to that cluster.
- The Serverless Spark UI page did not load: UNVERIFIED (docs re-read 2026-11-22:
  <https://docs.cloud.google.com/dataproc-serverless/docs/concepts/spark-ui>).
  The History Server's pages stand in for it.

## Console

Dataproc's menu:

- **Clusters**: every region's clusters.
  - **Create cluster** is a page of its own, in Google's steps:
    - Set up cluster: the name, region, zone, cluster type, image version,
      component gateway and autoscaling policy;
    - Configure nodes;
    - Customize cluster: scheduled deletion, properties and labels.
  - A cluster's page has Jobs, VM instances, Configuration and Web
    interfaces (the Spark History Server). Its actions are Submit job, Edit
    (worker nodes, autoscaling policy, labels), Start, Stop, Diagnose and
    Delete.
- **Jobs**: every region's jobs, and **Submit job**. A job's page has its
  Output (from Cloud Storage), Configuration (with its Spark UI) and Status
  history, with Stop and Delete.
- **Workflows**: workflow templates.
  - Create a template with a managed cluster or a cluster selector, its
    jobs and their prerequisites.
  - A template's page has Configuration, Jobs, Parameters and Workflows
    (its runs), with Run and Delete.
- **Autoscaling policies**: Create, Edit and Delete.

Dataproc's Serverless menu:

- **Batches**: every region's batches.
  - **Create batch** is a page of its own: the batch type's main file,
    arguments, files, runtime version, service account, properties and
    labels.
  - A batch's page has Summary, State history, Output (the driver output,
    read from Cloud Storage) and Job logs (the Cloud Logging filter),
    with Cancel and Delete. Summary's View Spark UI opens the batch in the
    History Server.
  - A PySpark notebook batch takes its notebook file and parameters.
- **Interactive sessions**:
  - Create a Spark Connect or a Jupyter session, from a template or a
    runtime version;
  - a session's page shows its Spark Connect endpoint and a PySpark line
    to connect with (for a Jupyter session, the line a BigQuery Studio
    notebook binds `spark` with), and View Spark UI;
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

- Jupyter sessions' Scala kernel, and a Jupyter kernel gateway endpoint.
- IPython magics in notebook batches.
- The optional components ZEPPELIN, DOCKER, RANGER, DELTA, ICEBERG and
  HUDI.
- Dataproc on GKE.
- YARN: MapReduce runs on Hadoop's local job runner, Hive on Tez local
  mode.
- Network settings (`subnetwork_uri`, `network_tags`), which are accepted
  and kept but have no effect: a local run has no VPC.
- Serverless autoscaling, which is not run. A batch's executors are
  `local[*]` threads of the driver's container.
