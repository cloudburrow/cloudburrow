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
  viewport size, and is never dismissible.

The indicator is not decoration. Someone with both a real console and this one open must be
able to tell which is which **without reading the data** — that is the test it has to pass.

---

## 3. Scope: screens exist only for operations that work

The console covers exactly what CloudBurrow supports, as
[compatibility.md](compatibility.md) records it.

| Area | In scope | Notes |
|---|---|---|
| Cloud Storage | Buckets list, bucket detail, objects and prefixes, create bucket; upload, download, preview and delete objects (#295) | A preview is plain text or a raster image, never the document an object claims to be; see below |
| Pub/Sub | Topics list, topic detail, subscriptions; on a topic: create subscription, publish message, pull and ack, pull without ack | A subscription has no address of its own. **Pull without ack** is labelled as changing delivery attempts, because Pub/Sub has no peek; pulled messages are shown in the dialog and never recorded in Activity (#294) |
| Cloud Tasks | Queues list, queue detail with configuration, tasks list, task detail | No queue edit: `UpdateQueue` is `Unimplemented` |
| Cloud Run | Services list, service detail, revision history, revision detail, deploy | Configuration is read-only; a change means deploying again |
| Kubernetes | Workloads, Pods, Services, Jobs, Nodes, Storage, Events — all read-only | CloudBurrow's own cluster. **Not project-scoped:** a Kubernetes object belongs to a namespace |
| Secret Manager | Secrets list, secret detail, version detail, enable/disable/destroy, create, add version, show value | A value is shown only on request, and the request is recorded in Activity |
| Resource Manager | Project list, project detail, labels editable, create, delete | A local registry, not Resource Manager: no organisations, folders, liens, IAM or billing |
| Firestore / Datastore | Collections and kinds, documents and entities, per-field detail, query builder | No create or delete: neither a collection nor a kind is a first-class resource |
| Bigtable | Tables, rows, per-cell detail, column families, row-range reader, create and delete tables | One fixed instance; the emulator has no instance administration |
| Request Log (Operations) | Every API call CloudBurrow served: time, service, method, resource, canonical code, duration. Filters by service, code and project, carried in the URL. Live over `/api/stream?stream=requests` (#291). | **Not a Google Cloud console screen.** It has no GCP reference, so no parity is claimed: it is modelled on LocalCloud's request logs and LocalStack's App Inspector. Only Cloud Tasks, Secret Manager and Cloud Run are observable, because CloudBurrow serves them itself. Storage, Pub/Sub and the opt-in emulators are listed as *requests not observable (direct port-forward to upstream emulator)*, never as an empty list. No payload or query string is recorded, and the page copies named fields only. |
| Spanner | Instances, databases, tables, columns and indexes, DDL, read-only query editor, create, drop | Node count is accepted and has no effect locally |
| Cloud SQL | Databases, tables, columns and indexes, schemas, views, functions, users, server settings, live activity, read-only SQL editor | A real PostgreSQL, not the Cloud SQL Admin API: no database-flags or user administration |
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

### 4.2 Resource list screens

- [x] Page title matching the documented console page name: **Buckets**, **Topics**, **Queues**, **Services**, **Secrets**, **Workloads**.
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
- [ ] Only fields CloudBurrow actually stores. A field the backend does not hold is absent, not blank. Held open deliberately: it is true screen by screen as far as it has been checked, and there is no test that would catch a new blank field, so checking it here would be claiming a guarantee nothing enforces.

### 4.5 Create forms

Fields and labels follow the documented forms, restricted to what CloudBurrow supports:

- [x] **Bucket**: Bucket name. Location is **absent** rather than fixed-and-shown: the backend has one location and a control that offered a choice it ignores would be a control that does nothing. The field's help says so.
- [x] **Topic**: Topic ID; **Add a default subscription** checkbox, defaulted on — a topic with no subscription drops every message published to it.
- [x] **Queue**: Queue name; Region, with its help saying CloudBurrow places nothing geographically.
- [x] **Service**: Service name; container image; environment variables; and every other field the adapter maps — port, entrypoint, arguments, CPU and memory limits, min and max instances, requests per instance, request timeout. Region is **absent**: the adapter takes one location from configuration, and `TestDeployFormOffersOnlyWhatTheAdapterMaps` asserts the form covers what `ToKnative` writes and nothing `Unsupported` refuses.
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
/tasks/queues                       queues
/tasks/queues/{queue}               queue detail
/run                                services
/run/{service}                      service detail
/run/create                         deploy a container
/secrets                            secrets
/secrets/{secret}                   secret detail: versions, configuration
/secrets/{secret}/{version}         version detail: state, and its value on request
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
/ai/{model}                         model provenance and execution
/logs                               the Logs Explorer
/activity                           the operations ledger
/requests                           the Request Log: API calls served
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
- **Pub/Sub subscriptions** open from their topic; a subscription has no address
  of its own because the topic is how anyone reaches it.
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
- [ ] A visible focus indicator on every focusable element. Table rows gained one with keyboard inspection; **not swept** across every control, and unchecked until it has been.
- [x] Focus moves into a dialog on open and returns to the trigger on close.
- [x] `Escape` closes any dialog or menu.
- [x] Landmarks: `banner`, `navigation`, `main`.
- [x] Tables use real `<table>` semantics with `<th scope="col">`.
- [x] Status changes announced through a live region.
- [ ] Text contrast at least 4.5:1 in both themes. **Not measured.** The tokens were chosen against the published palette, which is not the same as having computed the ratios, and a checked box here would be a claim about numbers nobody has taken.
- [x] Nothing conveyed by colour alone; a status has a label as well as a colour.

### Viewports

| Width | Behaviour |
|---|---|
| ≥ 1280px | Navigation docked beside the content |
| 960–1279px | Navigation closed, opening as an overlay above a scrim |
| < 960px | Navigation in an overlay; the cross-service search collapses to a control in the toolbar |
| every width | A table's own region scrolls horizontally; the page never does |

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
| Keyboard and focus behaviour | Test per §5 |
| Responsive behaviour | Rendered at each viewport in §5 |
| **Visual parity with GCP** | **Reference screenshots, which do not exist yet.** Stays `Partial`. |

[#49](https://github.com/cloudburrow/cloudburrow/issues/49) is where this checklist is
walked and the result recorded. Until then, every box above is unchecked — they describe
what is required, not what has been done.
