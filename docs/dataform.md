# Dataform

CloudBurrow serves Dataform v1 and v1beta1 (`dataform.googleapis.com`, #2105) with
`up --services dataform`, gRPC and JSON on `--port-dataform` (default
`9058`). `cloudburrow env` exports `CLOUDBURROW_DATAFORM_ENDPOINT`; pass it to
`option.WithEndpoint` with `option.WithoutAuthentication()`:

```go
c, err := dataform.NewClient(ctx, option.WithEndpoint(os.Getenv("CLOUDBURROW_DATAFORM_ENDPOINT")),
	option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

## How it runs

- **Compiling** uses Google's `@dataform/cli` (Apache-2.0, pinned in
  `dependencies.json`), installed once per instance into a container of the
  pinned Node image. A compile runs in that container with no network. A
  project whose `workflow_settings.yaml` names `dataformCoreVersion` gets
  that core version from an npm cache CloudBurrow fills first (that step
  reaches the npm registry); a project with a `package.json` needs
  `InstallNpmPackages` first, as on Google.
- **Invocations** run each action's SQL on the instance's BigQuery
  (`--services bigquery`), in dependency order, and report every action's
  state and SQL.
- **Git remotes** must be Secure Source Manager repositories this instance
  serves (Cloud Run's port): set `git_remote_settings.url` to the
  repository's `uris.git_https` and the token's Secret Manager version. A
  repository without a remote keeps its own git repository.

- **Customer-managed keys**: a repository's `kms_key_name` (or the
  region's `default_kms_key_name` in `UpdateConfig`) is a key of the
  instance's Cloud KMS (`--services kms`) in the repository's region. While
  its version is disabled or destroyed, calls on the repository's data are
  refused; the repository itself stays readable.
- **Developer Connect links**: `git_remote_settings.git_repository_link`
  may name a link of the instance's Developer Connect (Cloud Run's port) to
  the Secure Source Manager repository; its read-write token then
  authenticates the remote instead of a Secret Manager token.
- **Cancelling** an invocation stops the running action's BigQuery job.
- **Notebooks** (`notebook` actions in `definitions/actions.yaml`) run on
  the instance's notebook runtime, the Python kernel container BigQuery
  Studio's notebooks use (Docker required), with its BigQuery client
  pointed at the instance. Set `default_notebook_runtime_options`'
  `gcs_output_bucket` to a bucket of the instance's Cloud Storage
  (`--services storage`): the executed notebook is written to
  `gs://BUCKET/JOB_ID/FILE.ipynb`. Colab Enterprise runtime templates are
  not served.
- **Unit tests** (`type: "test"`) run only in an invocation whose
  `execution_mode` is `UNIT_TESTS_ONLY`, and fail with the first
  difference between the test's rows and the expected rows.
- **Data preparations** (`type: "dataPreparation"` SQLX, a pipe-syntax
  query) write their target in the load mode they name: `REPLACE_TABLE`,
  `APPEND`, `MAXIMUM`, `UNIQUE` (with `incrementalColumn`) or `MERGE`
  (with `uniqueKey`; BigQuery Studio's Upsert). The API's `LoadConfig` has
  no merge, so a MERGE action shows no `load` and the server keeps its
  keys; `generated_sql` shows the MERGE. A validation (`${EXPECT}`) sends
  the rows failing it to the error table, or fails the action when there
  is none. A YAML data preparation (`.dp.yaml`, declared in
  `actions.yaml`) runs its documented parts: each destination node gets
  its source table's rows; a node holding anything else (steps, whose
  format Google does not document) fails naming it.
- **Repository snapshots**: `gcs_repository_snapshot_destination`
  (`gs://BUCKET/PATH/`) gets each compilation's files as
  `COMPILATION_ID.tar.gz`.
- **`pipeline_config`** compiles a `DATAFORM` project in a folder, or a
  single `NOTEBOOK` or `SQL` file as one action (what BigQuery Studio's
  scheduled notebooks use).
- **SSH remotes**: `ssh_authentication_config` with the private key's
  Secret Manager version and the host's public key, against any git host
  you configure (on this machine, your network or the internet); git's
  ssh dials that host and checks it against that key alone. The
  instance's own Secure Source Manager serves git over HTTPS only.

## v1beta1

The same port serves `google.cloud.dataform.v1beta1.Dataform` (gRPC, and
JSON under `/v1beta1/`) over the same repositories: a repository made
through one version is read through the other. v1beta1 adds:

- **Branch management**: a workspace created with
  `enable_branch_management` can `CheckoutWorkspaceBranch` (creating one
  from `source_branch` if asked), `DeleteBranch`, and list its local and
  remote (`origin/<branch>`) branches with `FetchWorkspaceBranches`. Pull
  and push default to its checked-out branch.
- **`original_branch` and shallow clones**: a workspace starts from
  `original_branch` instead of the repository's default; with `depth` it
  is a clone of that many commits (`shallow` true), deepened by
  `SyncWorkspaceRefs` `deepen`. `FetchWorkspaceHistory` reports
  `reaches_shallow_boundary`.
- **Table update triggers**: a workflow config's `workflow_trigger_config`
  runs its release's latest compilation when the BigQuery tables it names
  change (any, or all of them, with `max_wait_duration` and
  `min_execution_duration`). Triggers are evaluated every 15 seconds from
  each table's `lastModifiedTime`, which the instance's BigQuery moves when
  a table's rows change; a change is seen only after the trigger is set.
- **Personal authentication**: `git_remote_settings.account_connector`
  names a Developer Connect account connector of the instance (Cloud Run's
  port); each caller's git runs with the token of the account they
  connected through it, from the instance's own sign-in page (no Google
  sign-in).
- **`DeleteRepositoryLongRunning`**, and the `folder_id` and
  `team_folder_id` `CreateFolder` and `CreateTeamFolder` take.

The console shows the same on Dataform's pages: Create development
workspace's advanced options, a workspace's Switch branch, Delete branch,
Fetch, Branches and Commit history, and a workflow configuration's table
update trigger and Trigger evaluations.

Docker and git are required on the host. Files live in the instance's
directory (`dataform/`); an ephemeral instance drops them at `up`.

What is not served is listed in [compatibility.md](compatibility.md#dataform--googleclouddataformv1) and its [v1beta1 section](compatibility.md#dataform-v1beta1--googleclouddataformv1beta1).
