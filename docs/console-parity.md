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
| Cloud Storage | Buckets list, bucket detail, objects and prefixes, create bucket; upload, download, preview and delete objects (#295); an object's own page (generation, metageneration, size, MD5 and CRC32C, headers, storage class, custom metadata, holds); on an object's row and its page **Copy**, **Move or rename**, **Edit metadata** and **Edit storage class**; on objects checked in a bucket or folder, **Compose** (#790) | A preview is plain text or a raster image, never the document an object claims to be; see below. An object's page is addressed `_details/<bucket>/<object>`, as Google's console addresses it, because an object and a folder can share a name; its crumbs are the bucket and folders. Copy is the official client's `Copier` (`objects.rewrite`), to this bucket or another; Move is `ObjectHandle.Move` (`objects.move`), within the bucket; Compose is the `Composer` (`objects.compose`), 1 to 32 sources in the order checked, which the dialog shows and can reorder; Edit storage class rewrites the object onto itself as a new generation (STANDARD, NEARLINE, COLDLINE or ARCHIVE). Edit metadata is `objects.patch` on the JSON API against the metageneration just read, for Content-Type, Cache-Control, Content-Disposition and custom metadata, which it replaces: a key removed in the form is removed. **Nothing is replaced unless asked:** Copy, Move and Compose send `ifGenerationMatch=0`, so an existing destination is refused with the API's precondition failure, shown on the form; **Replace the destination if it exists** lifts it, and the console then asks for the destination's name back before sending. Every action is read back through the official client (`TestConsoleStorageObjectOperations`); in headless Chrome, `TestStorageObjectMetadataAndCopyThroughTheForms` |
| Pub/Sub | Two pages under one drawer row, **Topics** and **Subscriptions**. Topics list, topic detail, subscriptions; on a topic: create subscription, publish message, pull and ack, pull without ack; on each of the topic's subscription rows: delete. Subscriptions list (subscription, topic, delivery type, ack deadline) from `ListSubscriptions`, subscription detail (delivery — push endpoint and its attributes, or the BigQuery, Cloud Storage or Bigtable target — dead-letter policy, retention, ordering, exactly-once, filter, expiration, retry policy, labels), and delete (#595); **Edit topic** on a topic's page and **Edit subscription** on a subscription's page (#786) | The Subscriptions screen lists every subscription of the project, including one whose topic was deleted, which no topic page reaches. It has no create form: a subscription is created on its topic's page. **Pull without ack** is labelled as changing delivery attempts, because Pub/Sub has no peek; pulled messages are shown in the dialog and never recorded in Activity (#294). A subscription's **Delete**, on the topic page's row or on the Subscriptions screen, asks for the subscription's name back and calls `DeleteSubscription` through the official client (#595). The edit forms offer only what the pinned emulator's `UpdateTopic` and `UpdateSubscription` apply, measured against it (#786): a topic's message retention; a subscription's push endpoint and attributes (pull↔push through `push_config`), ack deadline, retention, retain acknowledged messages, retry policy and dead-letter policy. **Labels are not offered on either form**: the emulator refuses them ("labels is not a known Topic field", and the same for a Subscription), and each form's note quotes that refusal. The topic, filter, expiration, message ordering and exactly-once delivery are shown disabled, each with the reason (the emulator refuses a filter or expiration change as "currently unsupported in the Pub/Sub Emulator"). A topic's retention, once set, cannot be emptied: the emulator would save 31 days. Through the official client against the emulator, read back by it (`TestConsolePubSubEditTopicAndSubscription`); the subscription form is driven in headless Chrome by `TestPubSubEditSubscriptionThroughTheForm` |
| Cloud Tasks | Queues list, queue detail with configuration, tasks list, task detail; create queue, pause, resume, purge, delete; **Edit queue** and **Create task** on a queue's page, **Delete task** on a task's (#784) | Edit queue changes what `UpdateQueue` applies here: rate limits (max dispatches per second, max concurrent dispatches; max burst size shown, output only) and retry parameters (max attempts, min and max backoff, max doublings, max retry duration). Create task is an HTTP target only: URL, method, headers, body, optional schedule time and task ID, through `CreateTask`. App Engine targets and routing, OIDC and OAuth tokens and Cloud Logging are not offered: the API refuses them as `UNIMPLEMENTED`. Both go through the in-process service's own API and are read back with the official client (`TestConsoleTasksEditQueueAndCreateTask`); the edit form is driven in headless Chrome by `TestTasksEditQueueThroughTheForm` |
| Cloud Run | Two pages under one drawer row, **Services** and **Jobs** (#785). Services list, service detail, revision history, revision detail, deploy, **Edit and deploy new revision** (#595), **Delete revision** on a revision that serves no traffic (#785). Jobs list, job detail (executions, configuration), **Create job**, **Edit job**, **Execute**, **Delete**; execution detail (status, task counts, conditions, the failure's message, Logs), **Cancel** while running, **Delete** when finished (#785) | Jobs go through the adapter's `Jobs` and `Executions` API, as the SDK does. The job form holds what the adapter maps (image, command, arguments quoted as a shell would, variables, task count, parallelism, retries, task timeout, CPU and memory); permissions, service accounts, VPC access, volumes, binary authorization and triggers are named as not offered, because the adapter refuses or does not serve them, and so is the task list, because the Tasks API is not served. Cancel is performed without a confirmation, as a form's Cancel discards without asking (#783); every delete asks for the name back. An execution's Logs tab is its task pods' output read with `kubectl logs`: the Cloud Run API has no log method. Delete revision is absent on the serving revision and on a service CloudBurrow did not create, which the API refuses.  The edit is the deploy form prefilled from the serving revision, with the service name shown and refused. It deploys through `UpdateService` and waits for the new revision; a failed rollout is reported with the adapter's message and the serving revision keeps serving. Labels, probes and secret-backed variables are read back and kept. Not offered for a multi-container service, or one whose entrypoint or arguments contain spaces, which the form's space-separated fields cannot hold. The label is not Google's: see §1 |
| Kubernetes | Workloads, Pods, Services, Jobs, Nodes, Storage, Events — all read-only | CloudBurrow's own cluster. **Not project-scoped:** a Kubernetes object belongs to a namespace |
| Secret Manager | Secrets list, secret detail, version detail, enable/disable/destroy, create, add version, show value | A value is shown only on request, and the request is recorded in Activity |
| Cloud KMS | Key rings (every location of the project), ring detail with keys, key detail with versions, version detail; create ring and key, add version, enable/disable/schedule destruction/restore, make primary, encrypt and decrypt (#593); **Edit key** on a key's page (#794) | Only symmetric ENCRYPT_DECRYPT keys at SOFTWARE protection exist locally, and the create form says so. Rings and keys cannot be deleted, as in Cloud KMS. **No key material is shown**: the provider calls only the API an SDK calls, which never returns it. A decrypted plaintext is shown in the dialog and recorded nowhere. Edit key changes the labels and the automatic rotation schedule (**Rotation period**, as `30d` or a Go duration, and **Next rotation time**, RFC 3339; #816), the fields `UpdateCryptoKey` applies here, sent with an update mask naming exactly the fields changed; the name is shown and fixed. The form does not check the period's range: one below the 24-hour minimum goes to `UpdateCryptoKey` and its refusal is shown. The service rotates the key at the next rotation time (a new version made primary) and moves it on by the period. Through the in-process service's own `UpdateCryptoKey`, read back with the official client (`TestConsoleKMSEditKey`, `TestKMSEditKeyRotationThroughUpdateCryptoKey`, the mask by `TestKMSEditKeyMaskNamesOnlyWhatChanged`); the form is driven in headless Chrome by `TestKMSEditKeyLabelsThroughTheForm` and `TestKMSEditKeyRotationThroughTheForm` |
| Cloud Scheduler | Jobs list with schedule, time zone, target, last run and result, next run; job detail; create (HTTP and Pub/Sub targets), pause, resume, run now, delete (#593); **Edit job** on a job's page (#795) | App Engine targets and OIDC or OAuth tokens are not offered: the API refuses them. Edit job changes what `UpdateJob` applies: description, schedule, time zone, the target's own fields (HTTP: URL, method, headers, body, attempt deadline; Pub/Sub: topic, message body, attributes) and the retry config. The name, region and target type are shown and fixed. An `Authorization` header, or one naming a token, secret or API key, is not put in the form and is kept. Not offered for a job whose body or message data is not UTF-8 text, which a text field cannot hold. Through the in-process service's own `UpdateJob`, read back with the official client (`TestConsoleSchedulerEditJob`); the form is driven in headless Chrome by `TestSchedulerEditJobThroughTheForm` |
| Resource Manager | Project list, project detail, labels editable, create, delete | A local registry, not Resource Manager: no organisations, folders, liens, IAM or billing |
| Firestore / Datastore | Collections and kinds, documents and entities, per-field detail, query builder; **Start collection**, **Add document**, and **Add field**, **Edit field**, **Delete field** and **Delete document** on their pages; **Create entity** on the Datastore screen and a kind's page, **Add property**, **Edit property**, **Delete property** and **Delete entity** (#796) | A collection or kind is not a first-class resource, so none is created or deleted as such: Start collection and Create entity make its first document or entity, and it goes with its last. Every write goes through the official `firestore` or `datastore` client against the emulator, and a refusal shows the emulator's or the client's own message (a taken ID is `ALREADY_EXISTS`, a latitude of 200 `INVALID_ARGUMENT`). Values are typed as Google's consoles type them — Firestore: string, number (with a decimal point a double, without one an integer), boolean, null, timestamp, geopoint, reference, map, array; Datastore: string, integer, float, boolean, timestamp, key, geopoint, array, embedded entity, null, each with **Exclude from indexes** — and a map, array or embedded entity is written as JSON whose values are strings, numbers, booleans, null, maps and arrays. A field is edited and deleted on its own page, prefilled with its stored type, so saving it unchanged writes the same type back (`TestEditFormsRoundTripTheStoredType`); a value that form cannot hold (bytes, or a map holding a timestamp, geopoint or reference) is shown without an edit and can be deleted. Only top-level fields and properties are edited, and only root entities in the default namespace are reached, which is all the screens list. Every delete asks for the name back and returns to the page above. A Firestore field edit carries the document's update time as a precondition and a Datastore change is a transaction, so a change made meanwhile is refused rather than overwritten. `TestConsoleFirestoreDocumentCreateEditDelete` and `TestConsoleDatastoreEntityCreateEditDelete` (emulators shard) read every write back through the official clients. **Not driven in a browser:** the emulators shard has no browser step |
| Bigtable | Tables, rows, per-cell detail, column families, row-range reader, create and delete tables | One fixed instance; the emulator has no instance administration. A table created with its column families is listed by the official admin client, and a console delete removes it (`TestConsoleBigtableCreateAndDeleteTable`, #699) |
| Request Log (Operations) | Every API call CloudBurrow served: time, service, method, resource, canonical code, duration. Filters by service, code and project, carried in the URL. Live over `/api/stream?stream=requests` (#291). | **Not a Google Cloud console screen.** It has no GCP reference, so no parity is claimed: it is modelled on LocalCloud's request logs and LocalStack's App Inspector. Cloud Tasks, Secret Manager, Cloud Run, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager and Cloud Storage are observable, because CloudBurrow serves them itself; storage calls are scraped from the in-cluster server with the bucket and object they were for (#518). Which services are observed comes from the observers `up` wires, the same set the metrics count (#683). Pub/Sub, the opt-in emulators, Memorystore and Cloud SQL are listed as *requests not observable (forwarded to the service's own server without being read)*, never as an empty list. No payload or query string is recorded, and the page copies named fields only. |
| Fault injection (Operations) | The rules `/admin/faults` holds (service, method glob, project, probability, code or HTTP status, latency, remaining of count, seed, injected); **Add rule** with exactly those fields, offering only the services the instance interposes and the codes and HTTP statuses the API maps; **Delete** per rule, confirmed by its id; **Clear all**, confirmed; the services whose rules are refused, each with the admin API's own reason; **Recent faults** from the recorder's `fault` events, refreshed every two seconds (#800) | **Not a Google Cloud console screen**, so no parity is claimed. It needs the instance's admin token, pasted once per tab, because a workload can reach the console on Docker Desktop and the console adds no token of its own ([networking.md](networking.md#the-console-and-the-admin-token)). Every action is the admin API's, called in process with that token, and a refusal shows its message. **Cancel** on the form discards without asking; Escape asks. `TestConsoleFaultsActThroughTheAdminAPI`, `TestConsoleFaultRuleFailsTheSDKCall`, `TestFaultRuleAddedAndDeletedThroughTheForm` |
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
- [x] A **navigation menu** listing only the services in §3, each linking to its list screen. Kubernetes Engine, Vertex AI, Pub/Sub and Cloud Run are one drawer row each with their pages inside, because a bare "Services" or "Jobs" row sitting beside Cloud Run is ambiguous with Cloud Run's own.
- [x] **Light / Dark / Same as device** themes, with **no page reload on change** — the documented behaviour.
- [x] The current service and screen are visibly marked in the navigation.
- [x] An **Activate terminal** control in the toolbar, Cloud Shell's place: a drawer at the foot of the page that can be resized from its grip (by pointer or arrow keys), minimised to its header and closed, and that reopens to the same shell until the shell ends. The drawer has **tabs**, as Cloud Shell's does (#834): **+** opens another shell in the same pod (files and tools are shared, as Cloud Shell's tabs share one VM), each tab is its own session with its own output and idle timeout, a tab is renamed by double-click or F2 and closed by its x or Delete (ending only its shell, with no confirmation), and tabs are chosen by click, the arrow keys in the tab strip, or Alt+PageUp / Alt+PageDown from inside the terminal. A reload or a closed drawer keeps every tab with its recent output; a console keeps at most 8 tabs and says so when asked for a ninth; a project switch is announced in every tab. The shell runs in a pod in the instance's cluster, never on this machine, scoped to the toolbar's project, and the drawer says when the project changes. When no shell can be had the drawer says why, never a blank terminal. What differs from Cloud Shell — no Google sign-in, no home directory beyond the pod, no Terraform, a read-only kubectl — is in [compatibility.md](compatibility.md#console).

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
- [x] **Topic edit** (#786): **Edit topic** on the topic's page, the **Topic ID** shown disabled and the **Message retention duration** prefilled, the one field the emulator's `UpdateTopic` applies. Labels are not offered: the emulator refuses them, and the form's note quotes its message. A retention once set is required, because emptying it would save 31 days. A value the emulator refuses, such as `5m`, is refused on the form with its message. `TestConsolePubSubEditTopicAndSubscription`.
- [x] **Subscription edit** (#786): **Edit subscription** on the subscription's page, prefilled from it, under **Delivery**, **Retry policy**, **Dead lettering** and **Not editable**. An empty push endpoint is a pull subscription, so giving one switches it to push and emptying it switches back; a BigQuery, Cloud Storage or Bigtable subscription is not offered the push fields. Only the fields that changed are sent, in one `UpdateSubscription`. The ID, topic, filter, expiration, message ordering and exactly-once delivery are shown disabled with the reason, and a request that changes one is refused; labels are not offered. `TestConsolePubSubEditTopicAndSubscription` reads each change back with the official client; in headless Chrome, `TestPubSubEditSubscriptionThroughTheForm`.
- [x] **Queue**: Queue name; Region, with its help saying CloudBurrow places nothing geographically.
- [x] **Queue edit** (#784): **Edit queue** on the queue's page, under **Rate limits** and **Retry parameters**, prefilled from the queue; **Max burst size** is shown disabled, because the API derives it. A value the API refuses (a max attempts below -1, a rate above 500) is refused with `UpdateQueue`'s own message on the form. `TestConsoleTasksEditQueueAndCreateTask`; in headless Chrome, `TestTasksEditQueueThroughTheForm`.
- [x] **Object actions** (#790), on an object's row menu and its page, each prefilled from the object: **Copy** (destination bucket, default this one; destination name; Replace), **Move or rename** (new name; Replace), **Edit metadata** (Content-Type, Cache-Control, Content-Disposition, custom metadata one `key=value` per line), **Edit storage class**; and **Compose** in a bucket's or folder's action bar, enabled once objects are checked, with the checked names prefilled one per line (destination name, Content-Type, Replace). **Replace the destination if it exists** is off by default: an existing destination is refused with the API's `412` message on the form, and checked, submitting asks for the destination's name to be typed back first, whose Cancel sends nothing. Cancel on the form itself discards without asking (#783). `TestConsoleStorageObjectOperations`; in headless Chrome, `TestStorageObjectMetadataAndCopyThroughTheForms`. In §8, `rewrite`, `move`, `compose` and `patch` are this surface; `objects.copy` and `objects.update` are excluded, being the one-request and replace-everything forms of edits made here through `rewrite` and `patch`.
- [x] **Task** (#784): **Create task** on the queue's page: URL, HTTP method (default POST), headers, body, schedule time and task ID, the last two optional. `TestConsoleTasksEditQueueAndCreateTask` reads the task back with the official client's `ListTasks` and receives the one due now at a test HTTP target.
- [x] **Document** (#796): **Start collection** on the Firestore screen (Collection ID, then the document's fields) and **Add document** on a collection's page: Document ID, optional, empty for an auto ID; a first field's name, type and value, optional. **Add field** on a document's page takes a name, type and value; **Edit field** on the field's page shows the name disabled and the type and value prefilled. The type is one of string, number, boolean, null, timestamp, geopoint, reference, map or array, refused on the form otherwise. `TestConsoleFirestoreDocumentCreateEditDelete` reads each back through the official `firestore` client with its type.
- [x] **Entity** (#796): **Create entity** on the Datastore screen (Kind, then the entity's fields) and on a kind's page: Key identifier, optional — a name, `id=123`, or empty for an allocated numeric ID; a first property's name, type, value and **Exclude from indexes**, optional. **Add property** and **Edit property** take the same, the edit with the name disabled and the rest prefilled. `TestConsoleDatastoreEntityCreateEditDelete` reads each back through the official `datastore` client with its type and index flag.
- [x] **Service**: Service name; container image; environment variables; and every other field the adapter maps — port, entrypoint, arguments, CPU and memory limits, min and max instances, requests per instance, request timeout. Region is **absent**: the adapter takes one location from configuration, and `TestDeployFormOffersOnlyWhatTheAdapterMaps` asserts the form covers what `ToKnative` writes and nothing `Unsupported` refuses.
- [x] **Service edit** (#595): the Service form's fields, prefilled from the revision that is serving — not the latest template, which after a failed rollout is the configuration that failed. **Service name** is shown disabled, and a request that changes it is refused with the field's own help text. `TestConsoleRunEditAndDeployNewRevision` changes a variable from the console and reads the new revision through the official client. In headless Chrome, `TestCloudRunEditFormIsPrefilledAndGivesFocusBack` opens the dialog from the service's page and reads the disabled name and the image and variable prefilled from the serving revision (#700).
- [x] **Job edit** (#795): **Edit job** on the job's page, the Create job form's fields prefilled from the job, under **Target** and **Retries**. **Name**, **Region** and **Target type** are shown disabled, and a request that changes one is refused; only the fields of the job's own target type are offered, plus the method, headers and attempt deadline of an HTTP target and the attributes of a Pub/Sub one, which Create job does not ask for. An invalid cron expression or time zone is refused with `UpdateJob`'s own message on the form. `TestConsoleSchedulerEditJob` reads the change back with the official client, and the next run on the new schedule; in headless Chrome, `TestSchedulerEditJobThroughTheForm`.
- [x] **Key edit** (#794): **Edit key** on a Cloud KMS key's page, the key's labels prefilled, one `key=value` per line, and **Key name** shown disabled; a request that changes it is refused. Labels and, since #816, the rotation period and next rotation time are offered, the fields `UpdateCryptoKey` applies here; purpose, protection level, algorithm and destroy scheduled duration are fixed, which the dialog's note says. The update mask names exactly the fields changed. A label key `UpdateCryptoKey` refuses (one with a capital), and a rotation period below its 24-hour minimum, are refused with its own message on the form. `TestConsoleKMSEditKey` reads the labels and the schedule back with the official client's `GetCryptoKey`; in headless Chrome, `TestKMSEditKeyLabelsThroughTheForm` and `TestKMSEditKeyRotationThroughTheForm`.
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
/run/{service}                      service detail: revision history, traffic, configuration
/run/{service}/{revision}           revision detail
/run/create                         deploy a container
/run/jobs                           Cloud Run jobs
/run/jobs/{job}                     job detail: executions, configuration
/run/jobs/{job}/{execution}         execution detail: conditions, configuration, logs
/run/jobs/create                    create a job
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
/faults                             fault injection: rules, add, delete, recent faults
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
form rather than resolving to a service called "create", and the longest
prefix wins, so `/run/jobs` is the Jobs page: a service called "jobs" is
listed but its page is not reachable by address. The previous
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

---

## 8. Feature parity: every implemented feature and its console surface

"Every feature we build should be accessible by the UI" (#782). The tables below place every
method a coverage page ([docs/coverage](coverage/README.md)) classes **Verified** or
**Implemented**, every `cloudburrow` command and every admin route in exactly one row, and each
row is one of three things: the console screen that offers it with the tests that exercise it; an
**exclusion** with the reason it has no screen; or a **gap** naming the open issue that builds it.

The tables are generated from `parityRows` in `cmd/cloudburrow/consoleparity_test.go`, and
`TestConsoleParityTables` fails when a coverage page gains a Verified method, the usage text a
command, or `internal/admin` a route, that no row claims, when a row names a method that is no
longer Verified, a provider the registry does not build or a test that does not exist, or when
this section drifts from the rows. Regenerate it with
`go test ./cmd/cloudburrow -run TestConsoleParityTables -update`. When a gap is built, its row
loses the issue and gains the screen and the test.

A screen named here offers what the Console column says because its provider's code offers it;
the Tests column names what exercises it, and an action with no test of its own there is offered
but not yet exercised from the console. Which opt-in services have a screen at all is §3, held by
`TestEveryServiceHasAParityRow`.

<!-- parity-table:begin (generated by TestConsoleParityTables; -update rewrites it) -->

| Resource | Methods (Verified) | Console | Tests |
|---|---|---|---|
| Cloud Tasks queues | `CloudTasks/CreateQueue`, `CloudTasks/DeleteQueue`, `CloudTasks/GetQueue`, `CloudTasks/ListQueues`, `CloudTasks/PauseQueue`, `CloudTasks/ResumeQueue`, `CloudTasks/PurgeQueue` | `tasks` screen: Queues list, queue detail with configuration, Create queue, Delete, Pause / Resume, Purge | `TestConsoleQueueActionsFollowState`, `TestNoDetailShowsABlankProperty`, `TestRowMenuClosesOnEscapeAndGivesFocusBack` |
| Cloud Tasks tasks | `CloudTasks/GetTask`, `CloudTasks/ListTasks`, `CloudTasks/DeleteTask` | `tasks` screen: Tasks section on the queue page, task detail (request, headers redacted, body), Delete task | `TestNoDetailShowsABlankProperty`, `TestTaskHeadersAreRedacted` |
| Cloud Tasks queue edit and task create | `CloudTasks/UpdateQueue`, `CloudTasks/CreateTask` | `tasks` screen: Edit queue (rate limits, retry parameters) and Create task (HTTP target) on a queue's page (#784) | `TestConsoleTasksEditQueueAndCreateTask`, `TestTasksEditQueueThroughTheForm` |
| Secret Manager secrets | `SecretManagerService/CreateSecret`, `SecretManagerService/DeleteSecret`, `SecretManagerService/GetSecret`, `SecretManagerService/ListSecrets`, `SecretManagerService/UpdateSecret` | `secrets` screen: Secrets list, secret detail, Create secret (with its first value), Edit (labels, annotations), Delete | `TestSecretCreateDoesNotLeaveAnEmptySecretBehind`, `TestSecretListingsNeverCarryPayloads`, `TestNoDetailShowsABlankProperty` |
| Secret Manager versions | `SecretManagerService/AddSecretVersion`, `SecretManagerService/AccessSecretVersion`, `SecretManagerService/EnableSecretVersion`, `SecretManagerService/DisableSecretVersion`, `SecretManagerService/DestroySecretVersion`, `SecretManagerService/GetSecretVersion`, `SecretManagerService/ListSecretVersions` | `secrets` screen: Versions section, version detail, Add version, Enable / Disable / Destroy, Show value (recorded in Activity) | `TestVersionActionsFollowTheVersionsState`, `TestRevealRefusesWhatTheAPIWouldRefuse` |
| Cloud Run services | `Services/CreateService`, `Services/DeleteService`, `Services/GetService`, `Services/ListServices`, `Services/UpdateService` | `run` screen: Services list, service detail, Deploy container, Edit and deploy new revision, Delete | `TestConsoleRunEditAndDeployNewRevision`, `TestRunEditDeploysTheFormThroughUpdateService`, `TestCloudRunEditFormIsPrefilledAndGivesFocusBack` |
| Cloud Run revisions | `Revisions/GetRevision`, `Revisions/ListRevisions` | `run` screen: Revision history and Traffic sections on the service page, revision detail | `TestNoDetailShowsABlankProperty`, `TestCloudRunRowSaysWhatIsServingAndWhyNot` |
| Cloud Run jobs | `Jobs/CreateJob`, `Jobs/DeleteJob`, `Jobs/GetJob`, `Jobs/ListJobs`, `Jobs/RunJob`, `Jobs/UpdateJob` | `run-jobs` screen: Jobs page (/run/jobs): jobs list, job detail with executions and configuration, Create job, Edit job, Execute, Delete (#785) | `TestConsoleRunJobsCreateExecuteAndCancel`, `TestRunJobsCreateExecuteCancelAndDeleteThroughTheAPI`, `TestRunJobEditKeepsWhatTheFormDoesNotShow`, `TestCloudRunJobCreatedExecutedAndDeletedInTheBrowser` |
| Cloud Run executions | `Executions/CancelExecution`, `Executions/DeleteExecution`, `Executions/GetExecution`, `Executions/ListExecutions` | `run-jobs` screen: Executions section on a job's page, execution detail (status, task counts, conditions, failure message, Logs), Cancel while running, Delete when finished (#785) | `TestConsoleRunJobsCreateExecuteAndCancel`, `TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs` |
| Cloud Run revision delete | `Revisions/DeleteRevision` | `run` screen: Delete revision on a Revision history row and a revision's page, for a revision that serves no traffic (#785) | `TestConsoleRunEditAndDeployNewRevision`, `TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts` |
| Cloud KMS key rings and keys | `KeyManagementService/CreateKeyRing`, `KeyManagementService/GetKeyRing`, `KeyManagementService/ListKeyRings`, `KeyManagementService/CreateCryptoKey`, `KeyManagementService/GetCryptoKey`, `KeyManagementService/ListCryptoKeys` | `kms` screen: Key rings list (every location), ring detail with keys, key detail, Create key ring, Create key | `TestConsoleKMSActsThroughTheAPIAnSDKSees`, `TestKMSCreateKeyRingThroughTheForm` |
| Cloud KMS key versions and crypto | `KeyManagementService/CreateCryptoKeyVersion`, `KeyManagementService/GetCryptoKeyVersion`, `KeyManagementService/ListCryptoKeyVersions`, `KeyManagementService/UpdateCryptoKeyVersion`, `KeyManagementService/DestroyCryptoKeyVersion`, `KeyManagementService/RestoreCryptoKeyVersion`, `KeyManagementService/UpdateCryptoKeyPrimaryVersion`, `KeyManagementService/Encrypt`, `KeyManagementService/Decrypt` | `kms` screen: Versions section, version detail, Add version, Make primary, Enable / Disable, Schedule destruction, Restore, Encrypt, Decrypt | `TestConsoleKMSActsThroughTheAPIAnSDKSees` |
| Cloud KMS key edit | `KeyManagementService/UpdateCryptoKey` | `kms` screen: Edit key on a key's page: labels (#794), rotation period and next rotation time (#816) | `TestConsoleKMSEditKey`, `TestKMSEditKeyThroughUpdateCryptoKey`, `TestKMSEditKeyRotationThroughUpdateCryptoKey`, `TestKMSEditKeyLabelsThroughTheForm`, `TestKMSEditKeyRotationThroughTheForm` |
| Resource Manager projects | `Projects/CreateProject`, `Projects/DeleteProject`, `Projects/GetProject`, `Projects/ListProjects`, `Projects/UpdateProject` | `projects` screen: Project registry list, project detail, Create project, Edit labels, Delete | `TestResourceManagerV3ListProjects`, `TestStateSaveResetLoadRestoresEverything`, `TestNoDetailShowsABlankProperty` |
| Resource Manager project search | `Projects/SearchProjects` | **Excluded:** the query form of ListProjects over the same registry; the Projects list and its filter show the same projects |  |
| Cloud Scheduler jobs | `CloudScheduler/CreateJob`, `CloudScheduler/DeleteJob`, `CloudScheduler/GetJob`, `CloudScheduler/ListJobs`, `CloudScheduler/PauseJob`, `CloudScheduler/ResumeJob`, `CloudScheduler/RunJob` | `scheduler` screen: Jobs list, job detail, Create job (HTTP and Pub/Sub targets), Pause / Resume, Force run, Delete | `TestConsoleSchedulerFollowsTheSDK`, `TestSchedulerPauseFromARow` |
| Cloud Scheduler job edit | `CloudScheduler/UpdateJob` | **Gap:** [#795](https://github.com/cloudburrow/cloudburrow/issues/795) |  |
| Cloud Logging entries | `LoggingServiceV2/ListLogEntries`, `LoggingServiceV2/ListLogs` | `/logs`: Logs Explorer: entries under source `logging/<log>`, filters in the URL, live stream | `TestLoggingWriteAndRead`, `TestFiltersNarrowTheView` |
| Cloud Logging writes | `LoggingServiceV2/WriteLogEntries` | **Excluded:** the application's write path; the Logs Explorer shows what was written |  |
| Cloud Logging log delete | `LoggingServiceV2/DeleteLog` | **Gap:** [#799](https://github.com/cloudburrow/cloudburrow/issues/799) |  |
| Pub/Sub topics | `Publisher/CreateTopic`, `Publisher/DeleteTopic`, `Publisher/GetTopic`, `Publisher/ListTopics`, `Publisher/ListTopicSubscriptions`, `Publisher/Publish` | `pubsub` screen: Topics list, topic detail with its subscriptions, Create topic (with a default subscription), Delete, Publish message | `TestConsoleCreatedTopicIsVisibleToTheOfficialSDK`, `TestSDKCreatedTopicAppearsInTheConsole`, `TestConsolePubSubActions` |
| Pub/Sub subscriptions | `Subscriber/CreateSubscription`, `Subscriber/DeleteSubscription`, `Subscriber/GetSubscription`, `Subscriber/ListSubscriptions`, `Subscriber/Pull`, `Subscriber/Acknowledge` | `pubsub-subscriptions` screen: Subscriptions list and detail, Delete; on a topic page: Create subscription, Pull and ack, Pull without ack | `TestConsolePubSubSubscriptionsScreen`, `TestConsolePubSubDeleteSubscription`, `TestConsolePubSubActions`, `TestSubscriptionsDeleteConfirmedByName` |
| Pub/Sub streaming pull and lease extension | `Subscriber/StreamingPull`, `Subscriber/ModifyAckDeadline` | **Excluded:** a subscriber client's transport and lease management; the console pulls with Pull and either acknowledges at once or leaves the messages unacked |  |
| Pub/Sub topic and subscription edit | `Publisher/UpdateTopic`, `Subscriber/UpdateSubscription`, `Subscriber/ModifyPushConfig` | **Gap:** [#786](https://github.com/cloudburrow/cloudburrow/issues/786) |  |
| Pub/Sub snapshots and seek | `Subscriber/CreateSnapshot`, `Subscriber/DeleteSnapshot`, `Subscriber/GetSnapshot`, `Subscriber/ListSnapshots`, `Subscriber/Seek`, `Publisher/ListTopicSnapshots` | **Gap:** [#787](https://github.com/cloudburrow/cloudburrow/issues/787) |  |
| Pub/Sub schemas | `SchemaService/CommitSchema`, `SchemaService/CreateSchema`, `SchemaService/DeleteSchema`, `SchemaService/DeleteSchemaRevision`, `SchemaService/GetSchema`, `SchemaService/ListSchemaRevisions`, `SchemaService/ListSchemas`, `SchemaService/RollbackSchema`, `SchemaService/ValidateMessage`, `SchemaService/ValidateSchema` | **Gap:** [#788](https://github.com/cloudburrow/cloudburrow/issues/788) |  |
| Cloud Storage buckets | `storage.buckets.insert`, `storage.buckets.delete`, `storage.buckets.get`, `storage.buckets.list` | `storage` screen: Buckets list, bucket detail (retention, lifecycle), Create, Delete | `TestConsoleCreatedBucketIsVisibleToTheOfficialSDK`, `TestCreateBucketThroughTheForm`, `TestConsoleStorageShowsRetentionAndLifecycle` |
| Cloud Storage objects | `storage.objects.list`, `storage.objects.get`, `storage.objects.insert`, `storage.objects.delete` | `storage` screen: Objects and prefixes in the bucket browser, Upload file, Download, Preview, Delete | `TestConsoleStorageObjects` |
| Cloud Storage storage layout | `storage.buckets.getStorageLayout` | **Excluded:** a capability read a client makes to choose its request paths (location and hierarchical namespace); it is no resource or action of its own |  |
| Cloud Storage bucket settings, retention lock, soft delete, managed folders | `storage.buckets.patch`, `storage.buckets.update`, `storage.buckets.lockRetentionPolicy`, `storage.buckets.restore`, `storage.objects.restore`, `storage.managedFolders.list` | **Gap:** [#789](https://github.com/cloudburrow/cloudburrow/issues/789) |  |
| Cloud Storage object copy, move, compose, rewrite, metadata | `storage.objects.rewrite`, `storage.objects.move`, `storage.objects.compose`, `storage.objects.patch` | `storage` screen: Object page (generation, metageneration, hashes, headers, custom metadata, holds); Copy, Move or rename, Edit metadata, Edit storage class on an object's row and page; Compose on checked objects; an existing destination replaced only when asked and confirmed (#790) | `TestConsoleStorageObjectOperations`, `TestStorageObjectMetadataAndCopyThroughTheForms`, `TestStorageObjectActionsThroughTheAPI` |
| Cloud Storage single-request copy and full metadata replace | `storage.objects.copy`, `storage.objects.update` | **Excluded:** the one-request and replace-everything forms of edits the console makes another way: Copy is objects.rewrite, as the official client's Copier sends it, and Edit metadata is objects.patch, which changes the fields the form holds without clearing the ones it does not show |  |
| Cloud Storage notifications | `storage.notifications.insert`, `storage.notifications.get`, `storage.notifications.list`, `storage.notifications.delete` | **Gap:** [#791](https://github.com/cloudburrow/cloudburrow/issues/791) |  |
| Cloud Storage HMAC keys and service account | `storage.projects.hmacKeys.create`, `storage.projects.hmacKeys.delete`, `storage.projects.hmacKeys.get`, `storage.projects.hmacKeys.list`, `storage.projects.hmacKeys.update`, `storage.projects.serviceAccount.get` | **Gap:** [#792](https://github.com/cloudburrow/cloudburrow/issues/792) |  |
| Cloud Tasks IAM policy | `CloudTasks/GetIamPolicy`, `CloudTasks/SetIamPolicy` | **Gap:** [#793](https://github.com/cloudburrow/cloudburrow/issues/793) |  |
| Secret Manager IAM policy | `SecretManagerService/GetIamPolicy`, `SecretManagerService/SetIamPolicy` | **Gap:** [#793](https://github.com/cloudburrow/cloudburrow/issues/793) |  |
| Cloud KMS IAM policy | `IAMPolicy/GetIamPolicy`, `IAMPolicy/SetIamPolicy` | **Gap:** [#793](https://github.com/cloudburrow/cloudburrow/issues/793) |  |
| Cloud Storage bucket IAM policy | `storage.buckets.getIamPolicy`, `storage.buckets.setIamPolicy` | **Gap:** [#793](https://github.com/cloudburrow/cloudburrow/issues/793) |  |
| Cloud Tasks permission check | `CloudTasks/TestIamPermissions` | **Excluded:** a caller asks which of the permissions it names it holds; CloudBurrow stores policies and enforces none, so there is nothing a page could show |  |
| Secret Manager permission check | `SecretManagerService/TestIamPermissions` | **Excluded:** a caller asks which of the permissions it names it holds; CloudBurrow stores policies and enforces none, so there is nothing a page could show |  |
| Cloud KMS permission check | `IAMPolicy/TestIamPermissions` | **Excluded:** a caller asks which of the permissions it names it holds; CloudBurrow stores policies and enforces none, so there is nothing a page could show |  |
| Cloud Storage permission check | `storage.buckets.testIamPermissions` | **Excluded:** a caller asks which of the permissions it names it holds; CloudBurrow stores policies and enforces none, so there is nothing a page could show |  |
| Firestore documents (read) | `Firestore/BatchGetDocuments`, `Firestore/RunQuery` | `firestore` screen: Collections, documents, per-field detail, query builder | `TestFirestoreTypeDistinguishesWhatAFlatCellCannot`, `TestQueryFormsCoverTheOperatorsTheyDocument` |
| Firestore transactions | `Firestore/BeginTransaction` | **Excluded:** session and transaction plumbing a client performs under a read or a write; no resource or action of its own |  |
| Firestore document writes | `Firestore/Commit` | `firestore` screen: Start collection, Add document (auto ID or named), Add field, Edit field and Delete field (typed: string, number, boolean, null, timestamp, geopoint, reference, map, array), Delete document | `TestConsoleFirestoreDocumentCreateEditDelete`, `TestEditFormsRoundTripTheStoredType` |
| Datastore entities (read) | `Datastore/Lookup`, `Datastore/RunQuery` | `datastore` screen: Kinds, entities, per-property detail, query builder | `TestDatastoreKeyRoundTripsWhatTheListingRendered`, `TestQueryFormsCoverTheOperatorsTheyDocument` |
| Datastore transactions | `Datastore/BeginTransaction` | **Excluded:** session and transaction plumbing a client performs under a read or a write; no resource or action of its own |  |
| Datastore entity writes | `Datastore/Commit` | `datastore` screen: Create entity (key name, id=N or auto ID), Add property, Edit property and Delete property (typed, with Exclude from indexes), Delete entity | `TestConsoleDatastoreEntityCreateEditDelete`, `TestEditFormsRoundTripTheStoredType` |
| Bigtable tables and rows (read) | `BigtableTableAdmin/CreateTable`, `BigtableTableAdmin/GetTable`, `BigtableTableAdmin/ListTables`, `Bigtable/ReadRows` | `bigtable` screen: Tables, column families, rows, per-cell detail, row-range reader, Create table, Delete | `TestConsoleBigtableCreateAndDeleteTable` |
| Bigtable column families and row writes | `BigtableTableAdmin/ModifyColumnFamilies`, `Bigtable/MutateRow` | **Gap:** [#797](https://github.com/cloudburrow/cloudburrow/issues/797) |  |
| Spanner instances, databases and reads | `InstanceAdmin/CreateInstance`, `InstanceAdmin/GetInstance`, `DatabaseAdmin/CreateDatabase`, `DatabaseAdmin/GetDatabase`, `DatabaseAdmin/GetDatabaseDdl`, `Spanner/ExecuteStreamingSql`, `Spanner/StreamingRead` | `spanner` screen: Instances, databases, tables, columns and indexes, DDL, read-only query editor, Create database, Drop database, Delete instance | `TestConsoleSpannerCreateAndDropDatabase` |
| Spanner sessions and transactions | `Spanner/CreateSession`, `Spanner/BeginTransaction` | **Excluded:** session and transaction plumbing a client performs under a read or a write; no resource or action of its own |  |
| Spanner DML | `Spanner/Commit` | **Gap:** [#798](https://github.com/cloudburrow/cloudburrow/issues/798) |  |

| Feature | Commands and admin routes | Console | Tests |
|---|---|---|---|
| Instance status | `status` | `/`: Dashboard: instance, cluster, endpoints, services, components | `TestStatusReportsLiveInstanceState` |
| Logs | `logs` | `/logs`: Logs Explorer: emulator, component and Cloud Run logs, live, filtered by source | `TestFiltersNarrowTheView`, `TestStreamSendsBacklogThenLiveEntries` |
| Admin events | `events`, `GET /admin/events` | `/requests`: Request Log: the recorder's request events, filtered by service, code and project, live (fault events: #800) | `TestTheRequestLogShowsServedCalls` |
| Fault injection | `POST /admin/faults`, `GET /admin/faults`, `DELETE /admin/faults` | **Gap:** [#800](https://github.com/cloudburrow/cloudburrow/issues/800) |  |
| State save and load, reset, seed | `state`, `reset`, `seed`, `POST /admin/state/export`, `POST /admin/state/import`, `POST /admin/reset`, `POST /admin/seed` | **Gap:** [#801](https://github.com/cloudburrow/cloudburrow/issues/801) |  |
| Connect, About and diagnose | `env`, `gcloud-setup`, `terraform`, `version`, `diagnose` | **Gap:** [#802](https://github.com/cloudburrow/cloudburrow/issues/802) |  |
| Starting the instance | `up` | **Excluded:** starts the process that serves the console, so the console cannot exist before it |  |
| Stopping and destroying | `stop`, `delete` | **Excluded:** ends the process that serves the console (stop) or destroys the cluster it runs on (delete); neither page could report its own outcome |  |
| Workstation checks and offline cache | `doctor`, `prefetch` | **Excluded:** run before an instance exists, to check prerequisites or fill the offline cache `up --offline` reads; a running instance's health is the dashboard's Components card |  |
| Trust and lifecycle hooks | `trust` | **Excluded:** trusting ./cloudburrow.json and its hooks lets `up` run host scripts; that decision stays at the terminal of whoever owns the checkout, so no page can grant it |  |
| Scripting and standalone tools | `wait`, `storage-server`, `gcloud-teardown`, `help` | **Excluded:** a script's exit status (wait), a server run without a console by design (storage-server), removing host gcloud configuration the console never writes (gcloud-teardown), and usage text (help) |  |

<!-- parity-table:end -->
