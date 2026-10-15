# Service Usage

CloudBurrow serves the Service Usage API v1 (`serviceusage.googleapis.com`, #1352): which APIs
each project has enabled. Terraform modules call it through `google_project_service`, often
before any other resource, and `gcloud services list`, `enable` and `disable` call it too.

It runs in the CLI process on `--port-serviceusage` (default `9023`, moved by `--port-base`), always,
as Resource Manager does. `cloudburrow env` exports `CLOUDBURROW_SERVICEUSAGE_ENDPOINT` and
`CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE`, `cloudburrow gcloud-setup` writes the `serviceusage`
override, and `cloudburrow terraform` sets the provider's `service_usage_custom_endpoint`. The
console's **APIs & Services** page lists a project's APIs, with **Enable** and **Disable**.

## What is listed

The catalogue is every API this instance serves, under its `googleapis.com` name:

| Always | With `--services` |
|---|---|
| `cloudresourcemanager.googleapis.com`, `serviceusage.googleapis.com` | `storage`, `pubsub`, `cloudtasks` (tasks), `run`, `secretmanager`, `cloudkms` (kms), `cloudscheduler` (scheduler), `logging`, `firestore`, `datastore`, `bigtable` and `bigtableadmin`, `spanner`, `bigquery` and `bigquerystorage`, `redis` (memorystore), `sqladmin` (cloudsql or cloudsql-mysql) |

Every listed API starts ENABLED in every project: a local instance serves each of them to every
project. A project's changes from that are stored in the project registry's store, so they are
kept exactly as the instance's projects are: in memory for an ephemeral instance, in the state
directory for a persistent one, and in state snapshots.

## Enabling what is not served is refused

Enabling an API outside the catalogue answers 400 FAILED_PRECONDITION with the reason:

- an API CloudBurrow serves, but this instance was not started with, names the `--services`
  value that would serve it (`start the instance with --services spanner`);
- any other API is one CloudBurrow does not serve at all.

A `batchEnable` that includes one changes nothing, as Google's fails as a whole. Answering ENABLED
for an API nothing serves would let a module apply and then fail at its first call, or worse,
send that call to Google.

## Disabling is stored, not enforced

Disabling an API records it as DISABLED for that project, and every read reports it so, but the API
keeps answering. This is the same choice [ADR-0006](adr/0006-iam-policy-surface.md) makes for IAM
policies:

- **What is stored is faithful.** A disable is persisted and returned exactly, so Terraform's
  `disable_on_destroy`, `gcloud services list --enabled` and any code that checks a project's
  enabled set behave as they do on Google.
- **Nothing is enforced.** Refusing calls to a disabled API would make every local service depend on
  a per-project switch that no test of the application itself is about, and a module destroyed with
  `disable_on_destroy` would break every other test sharing the instance. An application that must
  observe a disabled API failing needs a real project.
- `disableDependentServices` and `checkIfServiceHasUsage` are accepted. No API here depends on
  another, and no usage is tracked, so neither changes the result.

## Operations

Every change happens before the call returns, so `:enable`, `:batchEnable` and `:disable` return a
done operation, which `operations.get` answers (in memory, for the life of the process). Neither
Terraform nor gcloud polls one that is done. `operations.list`, `cancel` and `delete` are 501.

## Not served

- Folders and organizations as parents (501): the enabled set is kept per project.
- Consumer quota, `v1beta1`, `v2` and `v2beta`: gcloud's `services` commands that call them
  (`services quota`, `services policies`, group and dependency listings) are not served and fail
  locally, never at Google, through `gcloud-setup`'s override.

Per-method status is generated in [coverage/serviceusage.md](coverage/serviceusage.md); the claims
are in [compatibility.md](compatibility.md#service-usage--serviceusagegoogleapiscom-v1).
