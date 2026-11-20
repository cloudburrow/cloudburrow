# Service Usage

CloudBurrow serves the Service Usage API v1 (`serviceusage.googleapis.com`, #1352): which APIs
each project has enabled. Terraform modules call it through `google_project_service`, often
before any other resource, and `gcloud services list`, `enable` and `disable` call it too.

It runs in the CLI process on `--port-serviceusage` (default `9023`, moved by `--port-base`), always,
as Resource Manager does. `cloudburrow env` exports `CLOUDBURROW_SERVICEUSAGE_ENDPOINT` and
`CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE`, `cloudburrow gcloud-setup` writes the `serviceusage`
override, and `cloudburrow terraform` sets the provider's `service_usage_custom_endpoint`. The
console's **APIs & Services** pages (#1866) are Google's: Enabled APIs & services (charts and each
enabled API's requests, errors and latency: the project's own calls over Google's time ranges, from
the request history Cloud Monitoring serves as `serviceruntime.googleapis.com/api/*`), each API's
page with its graphs, **Select Graphs** and **Disable API**, the Library with **Enable**, and
Credentials.

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

## Service identities

`v1beta1` `services.generateServiceIdentity` (`gcloud beta services identity create`, and `gcloud
privateca roots create --kms-key-version` or `--bucket` before granting the Certificate Authority
Service agent its roles on the key and the bucket, #1834) answers a done operation
(`operations/finished.DONE_OPERATION`) whose `ServiceIdentity` is the service's *primary* service
agent in the project: Google defines primary service agents as "service agents whose email address
is returned when you trigger service agent creation"
([service account types](https://cloud.google.com/iam/docs/service-account-types#service-agents)),
and [Google's service agent list](https://cloud.google.com/iam/docs/service-agents) marks each
service's. The email is `service-PROJECT_NUMBER@DOMAIN` (the project's 12-digit number) with a
21-digit `uniqueId` that stays the same, as IAM's service account IDs are.

| API | Primary service agent domain (`.iam.gserviceaccount.com`) |
|---|---|
| `privateca.googleapis.com` | `gcp-sa-privateca` |
| `cloudkms.googleapis.com` | `gcp-sa-ekms` (the External Key Management Service agent, which [Create an EKM connection](https://cloud.google.com/kms/docs/create-ekm-connection) creates this way; the Cloud KMS Service Agent `gcp-sa-cloudkms` is not primary) |
| `pubsub.googleapis.com` | `gcp-sa-pubsub` |
| `secretmanager.googleapis.com` | `gcp-sa-secretmanager` ([Secret Manager CMEK](https://cloud.google.com/secret-manager/docs/cmek)) |
| `sqladmin.googleapis.com` | `gcp-sa-cloud-sql` ([Cloud SQL CMEK](https://cloud.google.com/sql/docs/mysql/configure-cmek)) |
| `cloudtasks.googleapis.com` | `gcp-sa-cloudtasks` |
| `cloudscheduler.googleapis.com` | `gcp-sa-cloudscheduler` |
| `run.googleapis.com` | `serverless-robot-prod` |
| `firestore.googleapis.com` | `gcp-sa-firestore` |
| `bigtableadmin.googleapis.com` | `gcp-sa-bigtable` |
| `spanner.googleapis.com` | `gcp-sa-spanner` |
| `redis.googleapis.com` | `cloud-redis` |
| `backupdr.googleapis.com` | `gcp-sa-backupdr` |
| `apikeys.googleapis.com` | `gcp-sa-apikeys` |
| `containeranalysis.googleapis.com` | `container-analysis` |
| `containerscanning.googleapis.com` | `gcp-sa-containerscanning` |

The other served APIs have no primary service agent in Google's list: `logging` (its agents,
`gcp-sa-logging` among them, are not primary; Logging names its CMEK account through
`getCmekSettings`), `storage` (`gs-project-accounts`, named through `projects.serviceAccount.get`),
`bigquery`, `bigquerystorage`, `datastore`, `bigtable` (the data API), `cloudresourcemanager` and
`serviceusage`. For them the operation's `ServiceIdentity` is empty, which gcloud's
`GenerateServiceIdentity` documents ("If service does not have a default identity, the response
will be an empty dictionary") and `services identity create` reports as "Service identity
created" with no email. An API CloudBurrow serves but has not looked up is 501 naming it, never a
guessed agent (a unit test fails when a newly served API is missing from both lists); an API the
instance does not serve is refused as `:enable` refuses it.

## Quotas

The Cloud Quotas API (`cloudquotas.googleapis.com` v1, #2030) is served on the Resource Manager
port. Its quota infos are the quotas CloudBurrow describes for the APIs it serves
(internal/service/cloudquotas/catalog.go), with Google's documented defaults; a quota preference is
granted at once, as no capacity review is needed locally, and is refused by Google's two safety
checks (a decrease below usage, or by more than 10%). The preferences are kept per project in the
project registry's store. A quota marked enforced is read through internal/quotalimit by the API
that enforces it, so a granted preference changes what that API allows: IAM's service accounts per
project (default 100) and Certificate Authority Service's CAs per project and region (default 1000).
Workflows' 10000 workflows per project is a system limit: listed, enforced, not adjustable. Rate
quotas are described but not applied. The console's IAM & Admin > Quotas & System Limits page and
each API's Quotas & System Limits tab read and edit them.

The Site Verification API (`siteVerification` v1) is on the same port; see
[compatibility.md](compatibility.md#site-verification--siteverification-v1).

## Not served

- Folders and organizations as parents (501): the enabled set is kept per project.
- Service Usage's consumer quota (its quotas are the Cloud Quotas API's, above), the rest of `v1beta1`, `v2` and `v2beta`: gcloud's `services` commands that call them
  (`services quota`, `services policies`, group and dependency listings) are not served and fail
  locally, never at Google, through `gcloud-setup`'s override.

Per-method status is generated in [coverage/serviceusage.md](coverage/serviceusage.md); the claims
are in [compatibility.md](compatibility.md#service-usage--serviceusagegoogleapiscom-v1).
