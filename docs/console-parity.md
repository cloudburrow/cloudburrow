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
- **Pixel parity is not specified and is not claimed.** Exact spacing, type scale, palette values, table row heights and icon metrics were **not** measured against a real console. Any checklist item asserting them would be invented, and [#43](https://github.com/identity-wael/cloudburrow/issues/43) explicitly forbids inferring unseen screens.
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
| Cloud Storage | Buckets list, bucket detail, objects list, create bucket | Notifications tab where configured |
| Pub/Sub | Topics list, topic detail, subscriptions list | |
| Cloud Tasks | Queues list, queue detail, tasks list | |
| Cloud Run | Services list, service detail, revisions, deploy | |
| Kubernetes | Workloads list, read-only | CloudBurrow's own cluster |
| Secret Manager | Secrets list, secret detail, versions | |
| Local AI | Nothing operational | See §6 |

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

- [ ] A **toolbar** across the top, the region the appearance documentation names.
- [ ] Product name **CloudBurrow** at the left of the toolbar, with a navigation-menu trigger beside it.
- [ ] A **project selector** in the toolbar showing the current project, opening a list of projects that hold resources.
- [ ] A **search** input in the toolbar.
- [ ] A **notifications** control showing in-progress and recent operations.
- [ ] A **Settings and utilities** control (`more_vert`), carrying at minimum the theme choice.
- [ ] A persistent, non-dismissible **`LOCAL`** indicator (§2).
- [ ] A **navigation menu** listing only the services in §3, each linking to its list screen.
- [ ] **Light / Dark / Same as device** themes, with **no page reload on change** — the documented behaviour.
- [ ] The current service and screen are visibly marked in the navigation.

### 4.2 Resource list screens

- [ ] Page title matching the documented console page name: **Buckets**, **Topics**, **Queues**, **Services**, **Secrets**, **Workloads**.
- [ ] A primary **create** action labelled as the documentation labels it — **Create**, **Create topic**, **Create queue**, **Deploy container** — with the `add_box` icon where documented.
- [ ] A table with a header row, sortable by name, with a filter input above it.
- [ ] Pagination, with the page size selectable.
- [ ] Row selection, and a delete action that is disabled until something is selected.
- [ ] A refresh control.

### 4.3 The four states every data screen must have

Not optional, and each is a separate check because each is separately easy to get wrong:

- [ ] **Loading** — a skeleton or progress indicator, never an empty table that looks like "none".
- [ ] **Empty** — says the collection is empty and offers the create action. Distinguishable from loading at a glance.
- [ ] **Error** — states what failed and offers retry. Never a silent empty table, which is the failure mode that makes a developer debug their own code.
- [ ] **Operating** — an in-flight create or delete shows progress, and the affected row shows it too.

### 4.4 Detail screens

- [ ] Resource name as the page title, with a breadcrumb back to the list.
- [ ] Tabs where the console documents tabs — Cloud Run's **Containers, Networking, Security** grouping is the documented example.
- [ ] Only fields CloudBurrow actually stores. A field the backend does not hold is absent, not blank.

### 4.5 Create forms

Fields and labels follow the documented forms, restricted to what CloudBurrow supports:

- [ ] **Bucket**: Bucket name; location fixed and shown as such.
- [ ] **Topic**: Topic ID; **Add a default subscription** checkbox.
- [ ] **Queue**: Queue name; Region.
- [ ] **Service**: Service name; Region; container image; environment variables.
- [ ] Validation inline and before submission, with the same constraint the API enforces.
- [ ] A submission failure shows the API's own message, not a generic one.

### 4.6 Operations and notifications

- [ ] A long-running operation appears in the notifications control.
- [ ] Its terminal state — success or failure — is shown, with the failure's cause.
- [ ] Nothing reports success before the API says so.

---

## 5. Routes, scoping, accessibility, viewports

### Routes

Deep-linkable, and readable as text:

```
/                                   dashboard
/storage/browser                    buckets
/storage/browser/{bucket}           objects
/pubsub/topics                      topics
/pubsub/topics/{topic}              topic detail
/tasks/queues                       queues
/run                                services
/run/{service}                      service detail
/secrets                            secrets
/kubernetes/workloads               workloads
```

Project and location are query parameters (`?project=`, `?location=`), so a link carries its
scope. A link opened with a project that has no resources shows the empty state for that
project, not another project's data.

### Accessibility

- [ ] Every control reachable by keyboard, in a sensible order.
- [ ] A visible focus indicator on every focusable element.
- [ ] Focus moves into a dialog on open and returns to the trigger on close.
- [ ] `Escape` closes any dialog or menu.
- [ ] Landmarks: `banner`, `navigation`, `main`.
- [ ] Tables use real `<table>` semantics with `<th scope="col">`.
- [ ] Status changes announced through a live region.
- [ ] Text contrast at least 4.5:1 in both themes.
- [ ] Nothing conveyed by colour alone; a status has a label as well as a colour.

### Viewports

| Width | Behaviour |
|---|---|
| ≥ 1280px | Navigation expanded beside the content |
| 960–1279px | Navigation collapsed to icons, expandable |
| < 960px | Navigation in an overlay; tables scroll horizontally rather than reflowing |

No layout below 360px is supported, and that is a stated limit rather than a silent break.

---

## 6. Local AI

[#39](https://github.com/identity-wael/cloudburrow/issues/39) established that local
inference is not viable today: no current Linux LiteRT-LM binary, and every Gemma artifact
gated. The console therefore shows **no AI panel that appears to work**.

The AI area is present, disabled, and says exactly why, linking to
[local-ai.md](local-ai.md). It becomes operational only when a runtime does — which is
[#48](https://github.com/identity-wael/cloudburrow/issues/48)'s to claim, not this
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

[#49](https://github.com/identity-wael/cloudburrow/issues/49) is where this checklist is
walked and the result recorded. Until then, every box above is unchecked — they describe
what is required, not what has been done.
