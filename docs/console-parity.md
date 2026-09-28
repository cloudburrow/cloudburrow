# Console parity specification

The requirement is a console that **looks like the Google Cloud console and works like it**,
not generic Material styling with a cloud theme. This page is the checklist that requirement
is judged against, and the record of what evidence it rests on.

It also states plainly what could **not** be evidenced, because a parity claim with no
reference behind it is a preference dressed as a requirement.

---

## 1. Evidence and its limits

### What was used

Public Google Cloud documentation, fetched **2026-09-21**. Each page carries its own
`Last updated` date, recorded below. These pages document console **navigation paths, page
names, control labels and form fields** — the structure of each screen and what a user can do
on it.

| Reference | Last updated | What it evidences |
|---|---|---|
| [Change the appearance of the Google Cloud console](https://docs.cloud.google.com/docs/get-started/console-appearance) | 2026-09-18 | The **toolbar** as a named region; **Settings and utilities** control; **Light / Dark / Same as device** themes; theme changes do not reload the page |
| [Create buckets](https://docs.cloud.google.com/storage/docs/creating-buckets) | 2026-09-18 | Cloud Storage → **Buckets** page; **Create** button; the **Create a bucket** form and its named sections and fields |
| [Create a topic](https://docs.cloud.google.com/pubsub/docs/create-topic) | 2026-09-18 | Pub/Sub → **Create topic** page; **Topic ID**, **Add a default subscription**, **Enable message retention**; **Create topic** button |
| [Deploy a Cloud Run service](https://docs.cloud.google.com/run/docs/deploying) | 2026-09-18 | Cloud Run page; **Deploy container** → **Create service** form; **Service name**, **Region**, **Authentication**, **Service scaling**, **Ingress**; the **Containers, Networking, Security** tab group |
| [Create Cloud Tasks queues](https://docs.cloud.google.com/tasks/docs/creating-queues) | 2026-09-18 | Cloud Tasks → **Queues** page; **Create queue** button; **Queue name**, **Region**; a green `check_circle` indicating a running queue |
| [Configure environment variables for services](https://docs.cloud.google.com/run/docs/configuring/services/environment-variables), fetched 2026-09-27 | 2026-09-24 | Changing an existing Cloud Run service: select it, edit on its **Containers** tab (**Variables & Secrets**), then **View diff & redeploy** → **Deploy changes**. [Deploy a Cloud Run service](https://docs.cloud.google.com/run/docs/deploying), also last updated 2026-09-24, describes the same flow |

**The Cloud Run edit is not labelled as Google's.** CloudBurrow's service page offers
**Edit and deploy new revision** (#595). The current reference above, dated 2026-09-24,
edits on the service page's own tabs and deploys with **View diff & redeploy**, and no dated
reference for the label CloudBurrow uses was found. So the label is CloudBurrow's own, no
parity is claimed for it, and the edit is a form over the deploy form's fields rather than
an imitation of the tabbed editor.

### What was **not** used, and the consequence

> **No authorized read-only console session was available, so no reference screenshots were
> captured.**

That has a specific and limiting consequence, stated here rather than buried:

- **Structural parity is specified and checkable**: which pages exist, how they are reached, what each is called, which controls they carry, and what those controls are labelled. Every item in §4 traces to a dated reference above.
- **Pixel parity is not specified and is not claimed.** Exact spacing, type scale, palette values, table row heights and icon metrics were **not** measured against a real console. Any checklist item asserting them would be invented, and [#43](https://github.com/cloudburrow/cloudburrow/issues/43) explicitly forbids inferring unseen screens.
- Screens with no reference above — billing, IAM, monitoring dashboards, the project chooser dialog's internals — are **out of scope** rather than approximated.

The support matrix therefore tracks **visual fidelity** separately from **API compatibility**,
and visual fidelity stays `Partial` until reference screens exist. Promoting it needs
screenshots, not a better-looking build.

### Assets and components

Original frontend code. No assumption is made that Google publishes the console frontend,
because it does not.

Where a published, permissively licensed Google asset exists it may be used and must be
recorded here with its licence.

**As built, no Google asset is shipped.** The icons are original SVGs authored for
CloudBurrow, and **no web font is loaded at all** — a font fetched at runtime would break the
offline requirement, and one vendored into the repository would add a binary asset and a
licence obligation for decoration. System font stacks are used instead.

**One third-party asset is vendored: the terminal emulator.** The terminal drawer (#781)
draws with [xterm.js](https://github.com/xtermjs/xterm.js) 6.0.0 and its fit addon 0.11.0,
MIT-licensed, copied unmodified from their npm packages into
`internal/console/assets/vendor/xterm/` with their licence texts, recorded in the licence
table of `internal/console/assets/icons/PROVENANCE.md`, in NOTICE and in dependencies.json,
and pinned by hash in `TestVendoredAssetsAreLicensed`. It is served from the binary and loaded
only when the drawer is first opened.

The documentation above names icons by their [Material Symbols](https://fonts.google.com/icons)
identifiers (`add_box`, `check_circle`, `more_vert`), which is the one place the console's own
iconography is publicly pinned down; those names informed which icons exist, not what ships.

**Material Design 3 is used as a published Google design system, and it is not asserted to be
the console's design system.** The console's is internal and unpublished. Using Material 3
gets the family resemblance honestly; it does not make the result a replica.

---

## 2. Branding and the local indicator

CloudBurrow branding, not Google's. Specifically:

- The product name in the toolbar is **CloudBurrow**, never "Google Cloud".
- No Google logo, wordmark or product logo is used anywhere.
- A **persistent `LOCAL` indicator** is visible in the toolbar on every screen, at every
  viewport size, and is never dismissible. `TestViewportsDrawerBadgeAndTableScroll` reads it from the rendered pixels at each width in §5.

The indicator is not decoration. Someone with both a real console and this one open must be
able to tell which is which **without reading the data** — that is the test it has to pass.

---

## 3. Scope: screens exist only for operations that work

The console covers exactly what CloudBurrow supports, as
[compatibility.md](compatibility.md) records it.

| Area | In scope | Notes |
|---|---|---|
| Cloud Storage | Buckets list, bucket detail, objects and prefixes, create bucket; upload, download, preview and delete objects (#295) | A preview is plain text or a raster image, never the document an object claims to be; see below |
| Pub/Sub | Two pages under one drawer row, **Topics** and **Subscriptions**. Topics list, topic detail, subscriptions; on a topic: create subscription, publish message, pull and ack, pull without ack; on each of the topic's subscription rows: delete. Subscriptions list (subscription, topic, delivery type, ack deadline) from `ListSubscriptions`, subscription detail (delivery — push endpoint and its attributes, or the BigQuery, Cloud Storage or Bigtable target — dead-letter policy, retention, ordering, exactly-once, filter, expiration, retry policy, labels), and delete (#595) | The Subscriptions screen lists every subscription of the project, including one whose topic was deleted, which no topic page reaches. It has no create form: a subscription is created on its topic's page. **Pull without ack** is labelled as changing delivery attempts, because Pub/Sub has no peek; pulled messages are shown in the dialog and never recorded in Activity (#294). A subscription's **Delete**, on the topic page's row or on the Subscriptions screen, asks for the subscription's name back and calls `DeleteSubscription` through the official client (#595) |
| Cloud Tasks | Queues list, queue detail with configuration, tasks list, task detail; create queue, pause, resume, purge, delete; **Edit queue** and **Create task** on a queue's page, **Delete task** on a task's (#784) | Edit queue changes what `UpdateQueue` applies here: rate limits (max dispatches per second, max concurrent dispatches; max burst size shown, output only) and retry parameters (max attempts, min and max backoff, max doublings, max retry duration). Create task is an HTTP target only: URL, method, headers, body, optional schedule time and task ID, through `CreateTask`. App Engine targets and routing, OIDC and OAuth tokens and Cloud Logging are not offered: the API refuses them as `UNIMPLEMENTED`. Both go through the in-process service's own API and are read back with the official client (`TestConsoleTasksEditQueueAndCreateTask`); the edit form is driven in headless Chrome by `TestTasksEditQueueThroughTheForm` |
| Cloud Run | Services list, service detail, revision history, revision detail, deploy, **Edit and deploy new revision** (#595) | The edit is the deploy form prefilled from the serving revision, with the service name shown and refused. It deploys through `UpdateService` and waits for the new revision; a failed rollout is reported with the adapter's message and the serving revision keeps serving. Labels, probes and secret-backed variables are read back and kept. Not offered for a multi-container service, or one whose entrypoint or arguments contain spaces, which the form's space-separated fields cannot hold. The label is not Google's: see §1 |
| Kubernetes | Workloads, Pods, Services, Jobs, Nodes, Storage, Events — all read-only | CloudBurrow's own cluster. **Not project-scoped:** a Kubernetes object belongs to a namespace |
| Secret Manager | Secrets list, secret detail, version detail, enable/disable/destroy, create, add version, show value | A value is shown only on request, and the request is recorded in Activity |
| Cloud KMS | Key rings (every location of the project), ring detail with keys, key detail with versions, version detail; create ring and key, add version, enable/disable/schedule destruction/restore, make primary, encrypt and decrypt (#593) | Only symmetric ENCRYPT_DECRYPT keys at SOFTWARE protection exist locally, and the create form says so. Rings and keys cannot be deleted, as in Cloud KMS. **No key material is shown**: the provider calls only the API an SDK calls, which never returns it. A decrypted plaintext is shown in the dialog and recorded nowhere |
| Cloud Scheduler | Jobs list with schedule, time zone, target, last run and result, next run; job detail; create (HTTP and Pub/Sub targets), pause, resume, run now, delete (#593) | App Engine targets and OIDC or OAuth tokens are not offered: the API refuses them |
| Resource Manager | Project list, project detail, labels editable, create, delete | A local registry, not Resource Manager: no organisations, folders, liens, IAM or billing |
| Firestore / Datastore | Collections and kinds, documents and entities, per-field detail, query builder | No create or delete: neither a collection nor a kind is a first-class resource |
| Bigtable | Tables, rows, per-cell detail, column families, row-range reader, create and delete tables | One fixed instance; the emulator has no instance administration. A table created with its column families is listed by the official admin client, and a console delete removes it (`TestConsoleBigtableCreateAndDeleteTable`, #699) |
| Request Log (Operations) | Every API call CloudBurrow served: time, service, method, resource, canonical code, duration. Filters by service, code and project, carried in the URL. Live over `/api/stream?stream=requests` (#291). | **Not a Google Cloud console screen.** It has no GCP reference, so no parity is claimed: it is modelled on LocalCloud's request logs and LocalStack's App Inspector. Cloud Tasks, Secret Manager, Cloud Run, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager and Cloud Storage are observable, because CloudBurrow serves them itself; storage calls are scraped from the in-cluster server with the bucket and object they were for (#518). Which services are observed comes from the observers `up` wires, the same set the metrics count (#683). Pub/Sub, the opt-in emulators, Memorystore and Cloud SQL are listed as *requests not observable (forwarded to the service's own server without being read)*, never as an empty list. No payload or query string is recorded, and the page copies named fields only. |
| Connect and About (Management tools) | What `cloudburrow env` exports for this running instance, in each format it prints (shell, plain, JSON, Terraform, docker-compose, Kubernetes), each with **Copy**, and the variables as a table; Go, Python and Node.js client snippets for the variables exported; the `eval "$(cloudburrow gcloud-setup)"`, `gcloud-teardown` and `cloudburrow terraform` commands to copy, each with what it writes on the host; **About**, version, commit and build date as `cloudburrow version` prints them, also in Settings and utilities; **Download bundle**, the `cloudburrow diagnose` bundle (#802). Reached from the toolbar's Connect control, where the console this mirrors keeps its help menu, and from the navigation | **Not a Google Cloud console screen**, so no parity is claimed. Every value comes from the functions the commands use, so the page cannot drift from them (`TestConnectVariablesAreEnvJSON`). No credential is shown: `MYSQL_PASSWORD` is named and withheld, the ADC fixture is shown by path only, and the admin token is never held. The console runs no command and writes no gcloud or Terraform configuration. The download needs the admin token, pasted once per tab, because the bundle carries the admin API's events and a workload can reach the console on Docker Desktop; the admin API decides (`TestConsoleDiagnoseNeedsTheAdminToken`). Below 600px the toolbar control gives way to the Settings and utilities link. `TestConsoleConnectIsTheInstancesEnv`, `TestConsoleDiagnoseBundleHoldsNoCredentials`, `TestConnectPageShowsTheEnvironmentAndDownloadsTheBundle` |
| Spanner | Instances, databases, tables, columns and indexes, DDL, read-only query editor, create (the instance is created when absent), **Drop database** on a database's page, **Delete instance** on an instance's page | Node count is accepted and has no effect locally. The database admin client reads back a console-created database and its first table, and each drop leaves `NOT_FOUND` (`TestConsoleSpannerCreateAndDropDatabase`, #699) |
| Cloud SQL for PostgreSQL | Databases, tables, columns and indexes, schemas, views, functions, users, server settings, live activity, read-only SQL editor; create database (`CREATE DATABASE`) and drop database (`DROP DATABASE`) | A real PostgreSQL, not the Cloud SQL Admin API: no database-flags or user administration. Dropping the database the server was initialised with is refused. A console-created database accepts a pgx connection, a console drop removes it from `pg_database`, and the refusal carries the provider's message (`TestConsoleCloudSQLCreateAndDropDatabase`, #699) |
| BigQuery | Datasets, tables, a table's schema (nested fields by dotted path), details and first rows (Preview, read with `tabledata.list`), and a read-only query editor on every dataset and table page (#698) | Through the official `cloud.google.com/go/bigquery` client against the forwarded REST port. **Read-only:** no create, delete or DML. The editor runs one `SELECT` or `WITH … SELECT`, checked before it is sent, because BigQuery has no read-only transaction and the emulator's dry run reports every statement as `SELECT` (measured); unqualified names resolve in the dataset on screen; at most 200 rows are shown and a query stops after 15 s. **One project:** the emulator serves the instance's default project only, so any other project shows a prompt naming that project rather than an error or an empty table (`TestConsoleBigQueryDatasetsSchemaAndQuery`) |
| Memorystore | **No screen** | There is no Memorystore admin API here: the backend is a Valkey server reached with any Redis client, and every console screen reads through a service's own API. A keyspace or `INFO` screen would be the console's own Redis client, not a Memorystore view, so none is offered. The dashboard lists the service and its endpoint |
| Cloud SQL for MySQL | **No screen** | The schema browser reads PostgreSQL's catalogue only (compatibility.md, "MySQL in the console"). Connect with a MySQL client |
| Cloud Logging | Entries written through the Logging API, in the Logs Explorer beside pod logs (source `logging/<log>`) | No screen of its own: no log buckets, sinks, views or metrics. See the Logs row |
| Monitoring | Node CPU, memory, pods, network rate, filesystem; per-pod CPU and memory; per-product log rate; time-range control | In memory only; a restart clears it |
| Logs | Live stream, severity timeline, filters in the URL, per-resource tab on every detail page | Severity is **inferred** from the line; container logs carry none |
| Local AI | Nothing operational | See §6. The Model Garden screen lists catalogued models and opens one for its provenance |

**An unsupported cloud feature is rendered as an explicit unavailable state**, never as a
working-looking control and never as a plausible number. Concretely, the following are
forbidden on any operational screen:

- A button that does nothing, or that opens a form which cannot succeed.
- A metric, chart, cost figure or count that is not read from live local state.
- A placeholder row, a lorem-ipsum resource, or a spinner that never resolves.
- A status badge whose value is not derived from the resource it describes.

An unavailable feature is shown disabled, with one sentence saying why and a link to the
matrix row that records it.

---

## 4. Screen-by-screen checklist

Each item is checkable against the references in §1. Items are **structural**; see §1 for why
none of them state pixel metrics.

### 4.1 Application shell — every screen

- [x] A **toolbar** across the top, the region the appearance documentation names.
- [x] Product name **CloudBurrow** at the left of the toolbar, with a navigation-menu trigger beside it.
- [x] A **project selector** in the toolbar showing the current project. It opens the **registry** rather than only projects that hold resources: scanning could not show a project with nothing in it and could not offer to make one, so the console could never answer "which projects are there". It also validates the selection — a `?project=` naming a project that is not registered used to leave every per-project screen reporting an error apiece with nothing saying the project was the problem.
- [x] A **search** input in the toolbar. It matches resource names, every column each list screen shows, product names and log entries, and shows results as you type.
- [x] A **notifications** control showing in-progress and recent operations.
- [x] A **Settings and utilities** control (`more_vert`), carrying at minimum the theme choice.
- [x] A persistent, non-dismissible **`LOCAL`** indicator (§2).
- [x] A **navigation menu** listing only the services in §3, each linking to its list screen. Kubernetes Engine and Vertex AI are one drawer row each with their pages inside, because a bare "Services" or "Jobs" row sitting beside Cloud Run is ambiguous with Cloud Run's own.
- [x] **Light / Dark / Same as device** themes, with **no page reload on change** — the documented behaviour.
- [x] The current service and screen are visibly marked in the navigation.
- [x] An **Activate terminal** control in the toolbar, Cloud Shell's place: a drawer at the foot of the page that can be resized from its grip (by pointer or arrow keys), minimised to its header and closed, and that reopens to the same shell until the shell ends. The shell runs in a pod in the instance's cluster, never on this machine, scoped to the toolbar's project, and the drawer says when the project changes. When no shell can be had the drawer says why, never a blank terminal. What differs from Cloud Shell — no Google sign-in, no home directory beyond the pod, no Terraform, a read-only kubectl — is in [compatibility.md](compatibility.md#console).

### 4.2 Resource list screens

- [x] Page title matching the documented console page name: **Buckets**, **Topics**, **Subscriptions**, **Queues**, **Services**, **Secrets**, **Workloads**.
- [x] A primary **create** action labelled as the documentation labels it — **Create**, **Create topic**, **Create queue**, **Deploy container** — from `Creator.CreateForm`, so the label comes from the provider that will perform it. The `add_box` icon is **not** used: the icon set here is the published product icon set plus line fallbacks, and CloudBurrow does not ship Material Symbols.
- [x] A table with a header row, sortable by any column, with a filter input above it.
- [x] Pagination, with the page size selectable — and, where a provider can continue a read, a control that fetches the rows past the backend's own bound rather than a note saying they exist.
- [x] Row selection, and a delete action that is disabled until something is selected.
- [x] A refresh control, plus a bounded automatic poll so a list does not go quietly stale.

### 4.3 The four states every data screen must have

Not optional, and each is a separate check because each is separately easy to get wrong:

- [x] **Loading** — a skeleton or progress indicator, never an empty table that looks like "none".
- [x] **Empty** — says the collection is empty and offers the create action **where there is one**. It used to say "Create one here" on every empty screen, including the read-only ones, naming a button that was not there and could not be. The listing's own note is kept on the empty path, which is the one path where it is the only thing on the page.
- [x] **Error** — states what failed and offers retry. Never a silent empty table, which is the failure mode that makes a developer debug their own code.
- [x] **Operating** — an in-flight create or delete shows progress, and the affected row shows it too.

### 4.4 Detail screens

- [x] Resource name as the page title, with one breadcrumb node per level, every one above the last a working link.
- [x] Tabs where the console documents tabs — Cloud Run's **Containers, Networking, Security** grouping is the documented example. The mechanism ships: `Driller` returns named sections, the strip carries `tablist`/`tab`/`tabpanel` semantics with arrow-key movement, and the selected tab is in the URL. Cloud SQL databases use it (**Tables** / **Schemas**); the other four drillable products declare one section each and correctly render no strip. Cloud Run's own grouping is a create-form grouping, and that one is `Section` on `Field` — see the Container section on `/run/create`.
- [x] Only fields CloudBurrow actually stores. A field the backend does not hold is absent, not blank. `TestNoDetailShowsABlankProperty` builds every screen from the same registry a running instance serves (`consoleProviders`), backs each with its in-process server or official fake (the built-in Cloud Storage, pstest, bttest, the Cloud Tasks, Secret Manager, KMS, Scheduler and Resource Manager services, and a fixture `kubectl` for Cloud Run and the cluster screens), seeds the sparsest resources each API accepts, opens every row and every openable row beneath it, and fails on any property whose value is empty or whitespace, naming the screen, the resource path and the label. A new screen is walked without being listed; the four whose backend is an emulator container (Firestore, Datastore, Spanner, Cloud SQL) are exempted by name with the reason. An unset field is left out, or worded for what unset means ("Not scheduled yet", "None: no revision is ready"), as unset scaling settings are; a label, header or attribute held with an empty value says "(empty value)" rather than vanishing. The server also drops any blank property before serving a page (`TestTheServerDropsBlankProperties`), which is the backstop for the four exempted screens.

### 4.5 Create forms

Fields and labels follow the documented forms, restricted to what CloudBurrow supports:

- [x] **Bucket**: Bucket name. Location is **absent** rather than fixed-and-shown: the backend has one location and a control that offered a choice it ignores would be a control that does nothing. The field's help says so.
- [x] **Topic**: Topic ID; **Add a default subscription** checkbox, defaulted on — a topic with no subscription drops every message published to it.
- [x] **Subscription**: not a form on the Subscriptions screen, which offers no create action. A subscription is created from its topic's page (**Create subscription**: Subscription ID, push endpoint, acknowledgement deadline), because a subscription belongs to a topic and a form that asked for the topic again would be one more place to name the wrong one.
- [x] **Queue**: Queue name; Region, with its help saying CloudBurrow places nothing geographically.
- [x] **Queue edit** (#784): **Edit queue** on the queue's page, under **Rate limits** and **Retry parameters**, prefilled from the queue; **Max burst size** is shown disabled, because the API derives it. A value the API refuses (a max attempts below -1, a rate above 500) is refused with `UpdateQueue`'s own message on the form. `TestConsoleTasksEditQueueAndCreateTask`; in headless Chrome, `TestTasksEditQueueThroughTheForm`.
- [x] **Task** (#784): **Create task** on the queue's page: URL, HTTP method (default POST), headers, body, schedule time and task ID, the last two optional. `TestConsoleTasksEditQueueAndCreateTask` reads the task back with the official client's `ListTasks` and receives the one due now at a test HTTP target.
- [x] **Service**: Service name; container image; environment variables; and every other field the adapter maps — port, entrypoint, arguments, CPU and memory limits, min and max instances, requests per instance, request timeout. Region is **absent**: the adapter takes one location from configuration, and `TestDeployFormOffersOnlyWhatTheAdapterMaps` asserts the form covers what `ToKnative` writes and nothing `Unsupported` refuses.
- [x] **Service edit** (#595): the Service form's fields, prefilled from the revision that is serving — not the latest template, which after a failed rollout is the configuration that failed. **Service name** is shown disabled, and a request that changes it is refused with the field's own help text. `TestConsoleRunEditAndDeployNewRevision` changes a variable from the console and reads the new revision through the official client. In headless Chrome, `TestCloudRunEditFormIsPrefilledAndGivesFocusBack` opens the dialog from the service's page and reads the disabled name and the image and variable prefilled from the serving revision (#700).
- [x] Validation inline and before submission, with the same constraint the API enforces.
- [x] A submission failure shows the API's own message, not a generic one.

### 4.6 Operations and notifications

- [x] A long-running operation appears in the notifications control.
- [x] Its terminal state — success or failure — is shown, with the failure's cause.
- [x] Nothing reports success before the API says so.

---

## 5. Routes, scoping, accessibility, viewports

### Routes

Deep-linkable, and readable as text:

```
/                                   dashboard
/products                           the product catalogue
/monitoring                         charts over the retained readings
/storage/browser                    buckets
/storage/browser/{bucket}           objects
/pubsub/topics                      topics
/pubsub/topics/{topic}              topic detail
/pubsub/subscriptions               subscriptions, every topic's
/pubsub/subscriptions/{sub}         subscription detail: delivery, dead lettering, configuration
/tasks/queues                       queues
/tasks/queues/{queue}               queue detail
/run                                services
/run/{service}                      service detail
/run/create                         deploy a container
/secrets                            secrets
/secrets/{secret}                   secret detail: versions, configuration
/secrets/{secret}/{version}         version detail: state, and its value on request
/kms/keyrings                       key rings, every location of the project
/kms/keyrings/{ring}                key ring detail: keys
/kms/keyrings/{ring}/{key}          key detail: versions, configuration
/kms/keyrings/{ring}/{key}/{v}      version detail: state, algorithm, destroy times
/scheduler/jobs                     jobs
/scheduler/jobs/{job}               job detail
/projects                           the project registry
/projects/{project}                 project detail: labels, scope
/kubernetes/workloads               Deployments, StatefulSets, DaemonSets, ReplicaSets
/kubernetes/workloads/{name}        workload detail: managed pods, revision history
/kubernetes/pods                    pods
/kubernetes/pods/{pod}              pod detail: containers, configuration, metrics, logs
/kubernetes/services                Kubernetes Services
/kubernetes/services/{name}         service detail: ports, endpoints
/kubernetes/jobs/{name}             job detail: the pods the job ran
/kubernetes/nodes/{node}            node detail: resources, conditions, taints
/kubernetes/storage/{claim}         persistent volume claim detail
/kubernetes/events                  events
/firestore/{collection}             documents
/firestore/{collection}/{document}  one document, field by field
/datastore/{kind}                   entities
/datastore/{kind}/{key}             one entity, property by property
/bigtable/{table}                   rows, and the table's column families
/bigtable/{table}/{rowKey}          one row, cell by cell
/spanner                            instances
/spanner/{instance}                 databases
/spanner/{instance}/{database}      tables, DDL, properties, query editor
/spanner/{instance}/{db}/{table}    columns and indexes
/cloudsql/{database}                database detail, and the SQL editor
/cloudsql/{database}/{table}        table detail: columns, indexes
/bigquery                           datasets, or the one-project prompt
/bigquery/{dataset}                 tables, details, query editor
/bigquery/{dataset}/{table}         schema, details, preview, query editor
/ai/{model}                         model provenance and execution
/logs                               the Logs Explorer
/activity                           the operations ledger
/requests                           the Request Log: API calls served
/connect                            Connect and About: env, gcloud-setup, terraform, version, diagnose
```

Every drillable route also carries `?tab=` for the section on screen, so a
colleague can be sent a tab rather than a resource. The Logs Explorer's filters
are in the query string too — `severity`, `source`, `resource`, `contains`,
`scope`, `operation` — because a log query someone got right is a log query
worth sending to someone else.

A resource lives at its own address, one path segment per level, so a table
inside a database is linkable and readable as text. The depth is the
provider's: `Driller.Detail` receives the ordered path, and one that does not
understand a level says so rather than quietly showing the level above.

An exact route wins over a resource path, so `/run/create` stays the deploy
form rather than resolving to a service called "create". The previous
`?resource=` form still resolves, so a link saved before this keeps working.

**What is still not drillable**, and why, since the previous note here named
three products that have had detail pages for several changes:

- **Events** and the Kubernetes **Services**/**Jobs** list rows that do open, but
  a *cluster event* does not: it is already the whole record, and a page showing
  one field per line would be the same text in a worse layout.
- **Pub/Sub subscriptions** do not open from their topic's page: a row there
  offers **Delete**, performed at the path `[topic, subscription]`, so a
  subscription of another topic cannot be deleted through this one. Each
  subscription's own page is on the Subscriptions screen,
  `/pubsub/subscriptions/{subscription}`, which also lists and deletes the ones
  whose topic was deleted (#595).
- A **Cloud Storage object** has no page of its own, but its row offers
  **Download**, **Preview** and **Delete**, and a bucket or folder offers
  **Upload file** (#295). Each streams through the official client: an upload
  goes to the backend as it is read and a download reaches the browser as it is
  produced, so no object is held in memory. Uploads are capped by the **Upload
  limit** in Settings (default 32 MiB, kept by the server that enforces it); a
  larger file is refused with the limit named and leaves no object. A download
  is always `application/octet-stream` with `Content-Disposition: attachment`.
  A **preview** (up to 1 MiB) is served as `text/plain` for text, JSON, XML,
  YAML, SVG and HTML, and as itself only for PNG, JPEG, GIF and WebP; every
  object response carries `Content-Security-Policy: default-src 'none'; …;
  sandbox` and `X-Content-Type-Options: nosniff`. Rendering an object as the
  HTML it claims to be would run its script with the console's authority, which
  can delete everything, so it is shown as its source. An upload replaces an
  existing object of the same name, as the API does.

Where a row does not open, it is text rather than a link: a link that leads
nowhere is worse than none. `Driller.Detail` receives the ordered path and a
provider that does not understand a level says so, so the refusal is visible
rather than silently collapsing onto the level above.

Project and location are query parameters (`?project=`, `?location=`), so a link carries its
scope. A link opened with a project that has no resources shows the empty state for that
project, not another project's data.

### Accessibility

- [x] Every control reachable by keyboard, in a sensible order. Table rows are inspectable with Enter or Space, which was mouse-only.
- [x] A visible focus indicator on every focusable element. The global `:focus-visible` rule draws a 2px accent ring, and `TestOutlineSuppressionHasAFocusVisibleRule` enumerates every selector that sets `outline: none` and fails unless a later `:focus-visible` rule for the same selector draws one back. The sweep found three that did not: the toolbar search field (whose `:focus` rule outranked the global ring, so it had none), `main` and the info panel. The ring's contrast against every surface it sits on is in `TestTokensMeetContrast`. This is a rule-level check; the browser suite (#594, part 1) is what will see the rendered ring.
- [x] Focus moves into a dialog on open and returns to the trigger on close. Asserted in headless Chrome by `TestDialogTakesFocusClosesOnEscapeAndGivesItBack` (the Create queue dialog, opened from the keyboard: focus on its first field, the page behind inert, Tab and Shift+Tab kept inside) and by `TestCloudRunEditFormIsPrefilledAndGivesFocusBack` for the Cloud Run edit dialog (#700).
- [x] `Escape` closes any dialog, the overlaid drawer and the collapsed search, and focus goes back to what opened it: the two dialog tests above and `TestViewportsDrawerBadgeAndTableScroll`.
- [x] `Escape` closes a row's actions menu and focus returns to the button that opened it, whether focus was on one of its items or still on that button. Asserted in headless Chrome by `TestRowMenuClosesOnEscapeAndGivesFocusBack` (a queue's row menu, opened from the keyboard; nothing is sent) (#771; found missing by #700).
- [x] Landmarks: `banner`, `navigation`, `main`.
- [x] Tables use real `<table>` semantics with `<th scope="col">`.
- [x] Status changes announced through a live region.
- [x] Text contrast at least 4.5:1 in both themes. `TestTokensMeetContrast` parses the light, dark and device-dark token blocks and computes the WCAG 2.x ratio of every text/background pairing the stylesheet uses, including hover layers and tinted fills (3:1 for focus rings and status marks; badge text is small, so 4.5:1). Measuring found five below the line, now fixed: light `--accent` `#1a73e8` → `#1967d2` (4.51:1 on white, under 4.5 on any tint), dark `--text-muted` `#9aa0a6` → `#a0a6ac` (4.35:1 on a hovered row), the dark error snackbar (white on `#f28b82`, 2.4:1), dimmed stale meter text (opacity .55, 2.4:1) and the selected picker item's id (opacity .8, 3.35:1). Borders are not measured: input-boundary contrast under 1.4.11 is a palette decision, not yet taken.
- [x] Nothing conveyed by colour alone; a status has a label as well as a colour.

### Viewports

| Width | Behaviour |
|---|---|
| ≥ 1280px | Navigation docked beside the content |
| 960–1279px | Navigation closed, opening as an overlay above a scrim |
| < 960px | Navigation in an overlay; the cross-service search collapses to a control in the toolbar |
| every width | A table's own region scrolls horizontally; the page never does |

Rendered at 1280, 1024, 800 and 360px by `TestViewportsDrawerBadgeAndTableScroll` (#700), which asserts each row of this table at its width, and that the LOCAL badge (§2) and the drawer's first link are painted: read from the pixels Chrome drew, since a check of the DOM passed on the drawer that drew nothing (console-verification.md). At 360px it found the page 442px wide — the project picker's narrow-width limits were outranked by its base rule, and the list's action bar could not wrap — and both are fixed.

No layout below 360px is supported, and that is a stated limit rather than a silent break.

**On the icon rail.** An earlier version of this table promised "navigation collapsed to
icons, expandable" between 960 and 1279px, and the stylesheet carried rules for a
`data-nav="collapsed"` state to match. Nothing could ever select them: `applyNavState` is
only ever given `closed`, `open` or `docked`. The rail was removed rather than built,
because the console this mirrors has no rail in that range either — the menu is simply
closed and opens as an overlay, which is what this code already did. A styled state the
JavaScript cannot enter is a trap for the next person auditing the drawer, so the rules went
with the promise.

---

## 6. Local AI

[#39](https://github.com/cloudburrow/cloudburrow/issues/39) established that local
inference is not viable today: no current Linux LiteRT-LM binary, and every Gemma artifact
gated. The console therefore shows **no AI panel that appears to work**.

The AI area is present, disabled, and says exactly why, linking to
[local-ai.md](local-ai.md). It becomes operational only when a runtime does — which is
[#48](https://github.com/cloudburrow/cloudburrow/issues/48)'s to claim, not this
specification's.

---

## 7. How each claim gets evidenced

Visual fidelity and API compatibility are **separate claims with separate evidence**, and
neither implies the other.

| Claim | Evidence required |
|---|---|
| A screen exists and is reachable | Route test |
| It shows live state | The resource is created through an SDK and appears; created in the UI and visible to the SDK |
| It has all four states (§4.3) | A test per state, including the error state under an injected backend failure |
| Keyboard and focus behaviour | Test per §5 (`TestKeyboardOpensAListRow`, `TestDialogTakesFocusClosesOnEscapeAndGivesItBack`) |
| Responsive behaviour | Rendered at each viewport in §5 (`TestViewportsDrawerBadgeAndTableScroll`) |
| **Visual parity with GCP** | **Reference screenshots, which do not exist yet.** Stays `Partial`. |

[#49](https://github.com/cloudburrow/cloudburrow/issues/49) is where this checklist is
walked and the result recorded. Until then, every box above is unchecked — they describe
what is required, not what has been done.
