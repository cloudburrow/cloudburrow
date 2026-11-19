# Service Usage

CloudBurrow serves the Service Usage API v1 (`serviceusage.googleapis.com`, #1352): which APIs
each project has enabled. Terraform modules call it through `google_project_service`, often
before any other resource, and `gcloud services list`, `enable` and `disable` call it too.

It runs in the CLI process on `--port-serviceusage` (default `9023`, moved by `--port-base`), always,
as Resource Manager does. `cloudburrow env` exports `CLOUDBURROW_SERVICEUSAGE_ENDPOINT` and
`CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE`, `cloudburrow gcloud-setup` writes the `serviceusage`
override, and `cloudburrow terraform` sets the provider's `service_usage_custom_endpoint`. The
console's **APIs & Services** pages (#1866) are Google's: Enabled APIs & services (charts and each
enabled API's requests, errors and latency, from the instance's own request counters), each API's
page with **Disable API**, the Library with **Enable**, and Credentials.

## What is listed

The catalogue is every API this instance serves, under its `googleapis.com` name:

| Always | With `--services` |
|---|---|
| `cloudresourcemanager.googleapis.com`, `serviceusage.googleapis.com`, `apikeys.googleapis.com` ([apikeys.md](apikeys.md)) | `storage`, `pubsub`, `cloudtasks` (tasks), `run`, `secretmanager`, `cloudkms` (kms), `cloudscheduler` (scheduler), `logging`, `firestore`, `datastore`, `bigtable` and `bigtableadmin`, `spanner`, `bigquery` and `bigquerystorage`, `redis` (memorystore), `sqladmin` (cloudsql or cloudsql-mysql), `backupdr` (backupdr, with cloudsql or cloudsql-mysql) |

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

## Disabling is enforced, as on Google

Disabling an API records it as DISABLED for that project, every read reports it so, and every call
to the API made in that project fails as Google fails it (#1914). This reverses the earlier choice
to store a disable without enforcing it: what runs locally should predict what runs on Google, and
an application, module or test that disables an API (Terraform's `disable_on_destroy`, `gcloud
services disable`, the console's **Disable API**) and then calls it must see the failure it will
see there. Nothing changes until someone disables an API: every served API stays ENABLED in every
project by default.

The failure is Google's `SERVICE_DISABLED`:

- gRPC `PERMISSION_DENIED`, or HTTP 403 with the JSON error body (`code`, `message`,
  `status: PERMISSION_DENIED`, `details`);
- the message `<Title> has not been used in project <number> before or it is disabled. Enable it by
  visiting <activationUrl> then retry. If you enabled this API recently, wait a few minutes for the
  action to propagate to our systems and retry.`, where the title is the API's (`Cloud Pub/Sub
  API`) and the activation URL is Google's
  (`https://console.developers.google.com/apis/api/<service>/overview?project=<number>`);
- a `google.rpc.ErrorInfo` with `reason: SERVICE_DISABLED`, `domain: googleapis.com` and metadata
  `service`, `consumer` (`projects/<number>`), `activationUrl`, `serviceTitle` and `containerInfo`
  (the number), then a `google.rpc.LocalizedMessage` (`en-US`) and a `google.rpc.Help` link
  ("Google developers console API activation"), as Google's error model carries them
  ([AIP-193](https://google.aip.dev/193), https://cloud.google.com/apis/design/errors). The
  official clients read them with `apierror` (`Reason()`, `Metadata()`). A Cloud Storage XML API
  request is answered `AccessDenied` with the message in `Details`.

A call's project is the one Google checks: its quota project (`x-goog-user-project`, which the
clients send for `option.WithQuotaProject` and gcloud's `--billing-project`) if it names one, else
the project its resource names, from the routing headers the official clients send
(`x-goog-request-params`, `google-cloud-resource-prefix`), the request path, a `project` parameter
(`buckets.list`, `buckets.insert`), the bucket's own project for Cloud Storage, or, for an
in-process service, the first `projects/...` name in the request message. A call that names no
project is not checked.

There is one decision and one check, made the way IAM enforcement makes its (#1879):

- the Service Usage server decides, from the stored states (`internal/apienable`'s `Decider`);
- the services CloudBurrow serves in process (Cloud Tasks, Cloud Run, Cloud Scheduler, Memorystore,
  Cloud Logging, Cloud KMS, Secret Manager, the Cloud SQL Admin API and the Backup and DR Service)
  take a gRPC interceptor and an HTTP wrapper over their JSON API, which ask it directly, so a
  disable takes effect at once;
- the servers that run in the cluster (Cloud Storage's builtin server, the Pub/Sub, BigQuery and
  Bigtable fronts, and the call fronts of Firestore, Datastore and Spanner) ask it at
  `/cloudburrow/v1/services:disabled` on the Resource Manager port, through `cloudburrow-host`,
  and keep the answer for one second. A disable or an enable reaches them within that second, as
  Google's own change takes a while to reach its servers; a test against them retries until it
  has.

Resource Manager and Service Usage themselves are not gated: disabling them is stored and
reported, but they keep answering, so the instance can always be put back.

### Dependent services

Google refuses to disable an API that enabled APIs depend on: `services.disable` fails
`FAILED_PRECONDITION`, "The service <service> is depended on by the following active service(s):
<services>; Please specify disable_dependent_services=true if you want to proceed with disabling all
services.", unless `disableDependentServices` is true, which disables it and every enabled API that
depends on it, directly or through another, together
(https://docs.cloud.google.com/service-usage/docs/reference/rest/v1/services/disable; `gcloud
services disable --force` sets it). CloudBurrow follows those rules over the dependencies Google
documents among the APIs it serves (`serviceUsageDependents`). Google documents none among them
today (the BigQuery API's documented dependents, such as Google Kubernetes Engine, are not served:
https://docs.cloud.google.com/bigquery/docs/enable-assets), so no disable is refused for one yet.
`checkIfServiceHasUsage` is accepted; no usage is tracked, so it changes nothing.

## Operations

Every change happens before the call returns, so `:enable`, `:batchEnable` and `:disable` return a
done operation, which `operations.get` answers (in memory, for the life of the process). Neither
Terraform nor gcloud polls one that is done. `operations.list` lists every kept operation, oldest
first, paged (`pageSize`, `pageToken`); a `filter`, or a `name` other than `operations`, is 501
saying so, as Google documents no filter grammar for this API. `operations.cancel` of a done
operation answers `{}` and leaves it done, as google.longrunning's cancel does; `operations.delete`
forgets one, so `get` and a second `delete` are 404, and the change it made stays (#1530). The
console's **Service Usage operations** page (`/apis/operations`) lists a project's operations with
Delete.

## Not served

- Folders and organizations as parents (501): the enabled set is kept per project.
- Consumer quota, `v1beta1`, `v2` and `v2beta`: gcloud's `services` commands that call them
  (`services quota`, `services policies`, group and dependency listings) are not served and fail
  locally, never at Google, through `gcloud-setup`'s override.

Per-method status is generated in [coverage/serviceusage.md](coverage/serviceusage.md); the claims
are in [compatibility.md](compatibility.md#service-usage--serviceusagegoogleapiscom-v1).
