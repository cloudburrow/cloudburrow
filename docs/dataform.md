# Dataform

CloudBurrow serves Dataform v1 (`dataform.googleapis.com`, #2105) with
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

Docker and git are required on the host. Files live in the instance's
directory (`dataform/`); an ephemeral instance drops them at `up`.

What is not served is listed in [compatibility.md](compatibility.md#dataform--googleclouddataformv1).
