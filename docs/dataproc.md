# Dataproc: clusters, jobs, workflows and Serverless for Apache Spark

CloudBurrow serves the Dataproc API (`dataproc.googleapis.com` v1):

- **Serverless for Apache Spark** (#2156; the product is being renamed
  Managed Service for Apache Spark): batch workloads, interactive Spark
  Connect and Jupyter sessions, and session templates;
- **Dataproc on Compute Engine** (#2162): clusters, jobs on them, workflow
  templates and autoscaling policies;
- the **Spark UI** of every workload, in Apache Spark's History Server.

- **Dataproc on GKE** (#2162): virtual clusters on the instance's
  Kubernetes Engine clusters.

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
  The session also runs a Jupyter Server of its kernels (an image the CLI
  builds on the runtime's `apache/spark` image, `internal/sparkproc/kernel`):
  PySpark (ipykernel) and Scala (Almond 0.14.5, BSD-3-Clause, with Spark's
  Scala Connect client `spark-connect-client-jvm` of the runtime's Spark:
  3.5.9 on 1.2, 2.2 and 2.3, 4.0.1 on 3.0 and 4.2.0 on 4.0), each `spark` a
  Spark Connect session of the session's server. `kernel` `SCALA` is
  served. Every package is pinned by hash: the PyPI packages by the
  `requirements-*.txt` locks (`pip --require-hashes`), every jar by the
  `*.lock` files' SHA-256.
- The kernel gateway (#2162), as Google's dataproc-jupyter-plugin uses it
  (`GatewayClient.url` `https://PROJECT_NUMBER-dot-REGION.kernels.googleusercontent.com`),
  is `http://DATAPROC_HOST/kernels/PROJECT-dot-REGION/` (PROJECT the ID or
  number). `api/kernelspecs` lists the region's Jupyter session templates
  (named by template ID, `display_name` the template's); `POST api/kernels`
  makes a session from the template and starts its kernel; `GET` and
  `DELETE api/kernels/ID`, `interrupt`, `restart` and the `channels`
  websocket are the kernel's. Deleting a kernel terminates its session.
  Google documents no `runtime_info.endpoints` key for it, so a session
  has none.

## Notebook and SparkR batches

- `pyspark_notebook_batch` (#2162) runs a Jupyter notebook (`.ipynb`) from
  Cloud Storage as the batch's PySpark driver. Its code cells run in order
  in one Python process on the runtime; markdown and raw cells are skipped.
  - `params` are set as papermill sets them: a cell of `NAME = "VALUE"`
    lines after the cell tagged `parameters`, or first when no cell is
    tagged. A key that is not a Python identifier is `INVALID_ARGUMENT`.
  - `python_file_uris`, `jar_file_uris`, `file_uris` and `archive_uris` are
    as for a PySpark batch.
  - Cells run in IPython 8.37.0 (`InteractiveShell.run_cell`, pure-Python
    wheels pinned by SHA-256 in `dependencies.json`, mounted on the
    driver's `PYTHONPATH`), so magics (`%time`, `%%writefile`, `%env`)
    and shell commands (`!` lines) work. A cell that raises fails the
    batch with `TYPE: MESSAGE (in the notebook's cell In[N])`.
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
    `INVALID_ARGUMENT`.
  - DELTA (3.2.1; 4.2.0 on 3.0), ICEBERG (1.6.1, or 1.10 with
    `dataproc:dataproc.iceberg.version=1.10`; 1.11.0 on 3.0, whose
    `default_iceberg` catalog is a REST catalog, `uri`
    `env:ICEBERG_REST_CATALOG_URI`: the Iceberg REST catalog of the Hive
    4.2.0 standalone metastore, `apache/hive` pinned by digest, on the
    master at port 9001) and
    HUDI (0.12.3 on 2.1, 0.15.0, 1.2.0 on 3.0): their jars from Maven
    Central, pinned by SHA-256, at the component's paths, with the
    component page's Spark properties. Every cluster's Spark has Google's
    Cloud Storage connector, so `gs://` paths reach the instance's Cloud
    Storage.
  - ZEPPELIN (0.10.1, `apache/zeppelin` pinned by digest, linux/amd64
    under emulation; on 3.0, 0.12.1, the patch release of 3.0's 0.12.0,
    which runs natively on arm64 too, its interpreter on the cluster's
    Spark 4.1 and Java 17 mounted from the cluster's image and `%pyspark`
    on the image's Python 3.9) runs on the master with its
    Spark interpreter on the cluster's Spark master, at the component
    gateway's `Zeppelin`. Measured on an arm64 Mac: the server starts in
    about 10 s once its image is pulled (3.7 GB compressed), a 2.2
    cluster with it is `RUNNING` in about 50 s, the first `%spark`
    paragraph (launching the interpreter) takes about 30 s and a `%md`
    one 4 s.
  - DOCKER (Docker 24.0 on 2.2 and earlier, 28.1 on 2.3 and 3.0, the
    official `docker:*-dind-rootless` image pinned by digest): a rootless
    Docker daemon beside each node, unprivileged (no `--privileged`:
    `/dev/net/tun` and seccomp, AppArmor and `systempaths` unconfined).
    Every node and driver has the `docker` CLI, `DOCKER_HOST` its node's
    daemon, so a job runs containers. Because those daemons run
    unconfined, DOCKER is **off unless the instance is started with
    `cloudburrow up --allow-unconfined-containers`**
    (`CLOUDBURROW_ALLOW_UNCONFINED_CONTAINERS=true`): without it, creating
    or starting a cluster with DOCKER is refused `FAILED_PRECONDITION`
    (HTTP 400) with a message that names the flag, and a DOCKER cluster an
    opted-in instance left running goes to `ERROR` with that message when
    an instance without it restarts it. See
    [security.md](security.md#unconfined-containers-opt-in-2162).
  - RANGER: `CreateCluster` checks what the Ranger component page
    documents (SOLR required, `dataproc:ranger.kms.key.uri` and
    `dataproc:ranger.admin.password.uri` required, `INVALID_ARGUMENT`
    otherwise). The admin password is read from Cloud Storage, decrypted
    with the instance's Cloud KMS and checked against the image version's
    password rules. The master then runs:
    - MySQL 8.0 (the Ranger database) and the Ranger Admin server and web
      UI on port 6080, the Component Gateway's `Ranger`, signed in to as
      `admin` with that password;
    - Solr's `ranger_audits` core, with Ranger's own audit schema, where
      the plugins write and the Admin server reads the access audit;
    - the Ranger services `hadoop-dataproc`, `hive-dataproc` and
      `yarn-dataproc` (the names Google's "Use Ranger with Kerberos" page
      shows), made when the Admin server first starts;
    - Ranger's Hive plugin in HiveServer2 (`hiveserver2-site.xml`,
      `ranger-hive-security.xml`, `ranger-hive-audit.xml`): every Hive
      query is authorized by the `hive-dataproc` policies, polled every 5
      seconds, and a denial is audited to Solr.
    - Ranger's YARN plugin in the ResourceManager (`yarn.acl.enable`,
      `yarn.authorization-provider` in `yarn-site.xml`,
      `ranger-yarn-security.xml`, `ranger-yarn-audit.xml`; its lib
      directory on the class path from a mount of the Ranger image): every
      queue access is authorized by the `yarn-dataproc` policies, and one
      no policy decides falls back to the queue's own ACL, as
      `ranger.add-yarn-authorization` does by default. A denial is
      audited to Solr.
    - Ranger's HDFS plugin in the NameNode, as `enable-hdfs-plugin.sh`
      installs it (`dfs.permissions.enabled` and
      `dfs.namenode.inode.attributes.provider.class` =
      `RangerHdfsAuthorizer` in `hdfs-site.xml`,
      `ranger-hdfs-security.xml`, `ranger-hdfs-audit.xml`): every HDFS
      access is authorized by the `hadoop-dataproc` policies, and one no
      policy decides falls back to HDFS's own permissions
      (`xasecure.add-hadoop-authorization`). A denied read fails with
      Hadoop's `Permission denied: user=root, access=READ` and is audited
      to Solr.
    The image (`internal/sparkproc/ranger/Dockerfile`) is built on this
    machine, the first time a RANGER cluster starts, from the pinned Apache
    Ranger 2.8.0 source release. Ranger's own reactor does not build as
    published (its Kylin plugin needs an artifact only repo.spring.io
    serves, which answers 401), so CloudBurrow's two build files build
    only the modules the Admin server and the HDFS, Hive and YARN plugins
    need, with Ranger's own assembly descriptors; one source patch makes
    the Hive plugin, built for Hive 3.1.3, run in the cluster's Hive 4.0.1
    HiveServer2 (`ranger/hive4.patch`); every file Maven resolves is
    pinned by SHA-256 (`ranger/m2.sha256`).
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

## Dataproc on GKE

A virtual cluster (#2162, `virtual_cluster_config`) runs Spark on a
Kubernetes Engine cluster of the instance (a kind cluster; the `gke`
service must be on):

- `gke_cluster_target` is `projects/P/locations/L/clusters/C`, a cluster of
  the request's project; each `node_pool_target` names one of its node
  pools with its roles (`DEFAULT`, `CONTROLLER`, `SPARK_DRIVER`,
  `SPARK_EXECUTOR`), exactly one of them `DEFAULT`. A pool that does not
  exist is created from its `node_pool_config`, as Dataproc creates it.
- `kubernetes_software_config.component_version` gives `SPARK`:
  `3.5-dataproc-28`, `-22` or `-17`, or `3`, `3.5`, `dataproc-2.2` and
  `latest` for the newest. Its `spark:` properties are every job's Spark
  defaults.
- `kubernetes_namespace` defaults to the cluster's name. The namespace is
  made on the GKE cluster, labelled with the cluster's name and UUID, with
  a `spark` service account bound to the `edit` role.
- `staging_bucket` defaults to the region's staging bucket.
- Each job's driver is a pod in the namespace, on the `SPARK_DRIVER` pool
  (or the `DEFAULT` one), running `spark-submit` in client mode against
  `k8s://https://kubernetes.default.svc`; its executors are pods on the
  `SPARK_EXECUTOR` (or `DEFAULT`) pool. The image is the `apache/spark`
  3.5.9 image (its R variant for SparkR), which the nodes pull by digest.
  The driver's output, from its pod's log, is the job's
  `driver_output_resource_uri`.
- Spark, PySpark, SparkR and Spark SQL jobs run, as Google's overview
  lists them; other job types are `INVALID_ARGUMENT`. A virtual cluster
  cannot be updated, stopped or started (`INVALID_ARGUMENT`); deleting it
  deletes its namespace.

## Jobs

- `SubmitJob` and `SubmitJobAsOperation` run a job on the cluster its
  `placement` names, by `cluster_name` or `cluster_labels`. The driver is a
  container in client mode against the cluster's master.
  - PySpark, Spark, SparkSql (a query file or a query list, with
    `script_variables`) and SparkR jobs run.
  - Hadoop, Hive and Pig jobs run in the official `apache/hive` image
    (Hadoop 3.3.6, Hive 4.0.1, Tez) with Google's Cloud Storage connector
    for Hadoop, `gs://` reaching the instance's Cloud Storage through the
    cluster's relay. Hadoop MapReduce and Pig 0.18.0 (`-x mapreduce`) run
    on YARN: a ResourceManager, the MapReduce JobHistory Server and the
    Timeline Server on the master and a NodeManager on each worker (on the
    master of a single node cluster), the Capacity Scheduler's `default`
    queue; `yarn:` properties set `yarn-site.xml`. With the component
    gateway, `YARN ResourceManager`, `MapReduce Job History` and `YARN
    Application Timeline` are in `http_ports`. Hive jobs run Beeline
    against the cluster's HiveServer2, their queries on Tez on YARN.
  - HDFS (#2186), as Google's images run it: the NameNode on the master
    (RPC `CLUSTER-m:8020`, web UI 9870, the component gateway's `HDFS
    NameNode` at `hdfs/dfshealth.html`) and a DataNode on each primary
    worker (on the master of a single node cluster), all Hadoop 3.3.6 from
    the pinned `apache/hive` image (by digest: the Hadoop the cluster's
    YARN, Hive and Tez run). `fs.defaultFS` is `hdfs://CLUSTER-m` for every
    node, driver and job, so a path without a scheme, `hdfs:///` and
    `hdfs://CLUSTER-m/` are the cluster's HDFS in Hadoop, Hive, Pig and
    Spark jobs alike. `dfs.replication` is 2 (1 with fewer than two
    workers) and `dfs.permissions.enabled` is false (true with RANGER,
    below); `hdfs:` properties set `hdfs-site.xml`. The NameNode's
    metadata and the DataNodes' blocks are on their node's own disk
    (Dataproc's `/hadoop/dfs/name` and `/hadoop/dfs/data`), each
    DataNode's capacity the workers' `disk_config.boot_disk_size_gb`
    (500 GB by default, as the API reference has it). What Dataproc keeps
    in HDFS is there: the MapReduce staging and JobHistory directories
    (`/tmp/hadoop-yarn/staging`), a Spark job's staging directory
    (`/user/root/.sparkStaging`; jobs run as `root`, as Dataproc's do),
    Hive's and Tez's scratch directories, and the Hive warehouse
    (`/user/hive/warehouse`). Each node's disk is a directory of the
    instance's (`<state>/<instance>/dataproc-disks/<cluster UUID>/<host>`,
    mounted at `/hadoop`), so HDFS keeps its data while the cluster is
    stopped and started and across the instance's own `stop` and `up` in
    persistent mode (#2188), as Compute Engine persistent disks do; it
    loses it when the cluster is deleted (an ephemeral instance's clusters
    and disks start afresh). A scale down decommissions the removed
    workers' DataNodes before removing them and their disks.
    `dfs.permissions.enabled` is false also with Kerberos (Google's own
    kerberized data lake example turns it on in an initialization action),
    true with RANGER (Ranger's `enable-hdfs-plugin.sh`).
  - HDFS High Availability (#2188), as [Google's High Availability
    page](https://docs.cloud.google.com/dataproc/docs/concepts/configuring-clusters/high-availability)
    has it: a cluster with `master_config.num_instances` 3 has the masters
    `CLUSTER-m-0`, `-m-1` and `-m-2`; NameNodes `nn0` on `-m-0` and `nn1` on
    `-m-1`, each with a ZKFailoverController, a JournalNode (8485) and a
    ZooKeeper server (the ensemble, also the ZOOKEEPER component's) on
    each of the three masters, and no Secondary NameNode. `fs.defaultFS`
    is the nameservice `hdfs://CLUSTER` (`dfs.nameservices`,
    `ConfiguredFailoverProxyProvider`, `ha.zookeeper.quorum`), and when the
    active NameNode's master stops, the other NameNode becomes active.
    The ResourceManager and the other master daemons run on `-m-0`.
  - Local SSDs (#2188), as [Google's local SSDs
    page](https://docs.cloud.google.com/dataproc/docs/concepts/compute/dataproc-local-ssds)
    has them: a node's `disk_config.num_local_ssds` (0 to 8;
    `local_ssd_interface` `scsi` or `nvme`) are mounted at `/mnt/1`,
    `/mnt/2`, …; the DataNode's volumes are `/mnt/N/hadoop/dfs/data`, each
    of a local SSD's 375 GB, and the NodeManager's local directories (YARN's
    scratch and shuffle) `/mnt/N/hadoop/yarn/nm-local-dir`, unless
    `dataproc:dataproc.localssd.mount.enable` is false.
  - Spark, PySpark, SparkR and Spark SQL jobs run on YARN, as on Google's
    images: `master` is `yarn`, in client mode by default and in cluster
    mode with `spark.submit.deployMode=cluster` (a Spark SQL job runs in
    client mode). Each worker's NodeManager runs in the cluster's Spark
    image, so its containers have the image's Java, Python and R.
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
- `yarn_applications` holds the Spark application, its `tracking_url` the
  application's page in the cluster's History Server; for Hadoop and Pig
  jobs, their YARN applications, `tracking_url`
  `http://CLUSTER-m:8088/proxy/APPLICATION_ID/`.
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

- YARN ResourceManager High Availability: a High Availability cluster's
  ResourceManager runs on `-m-0` only (Google runs one on each master).
  Local SSDs are directories on the host's one disk, so they are not
  faster than the boot disk.
- A Dataproc on GKE cluster's `auxiliary_services_config` (a Spark History
  Server and a metastore of other clusters) is kept but not used, and its
  jobs' code reaches Cloud Storage only through the job's files.
- Network settings (`subnetwork_uri`, `network_tags`), which are accepted
  and kept but have no effect: a local run has no VPC.
- Serverless autoscaling, which is not run. A batch's executors are
  `local[*]` threads of the driver's container.
