/* CloudBurrow console.
 *
 * No framework and no build step: the assets ship as they are written, which
 * is what makes "no external CDN after installation" true by construction
 * rather than by a bundler configuration nobody checks.
 *
 * Every screen reads live state from /api. There is no fixture data here, on
 * purpose: a placeholder row is indistinguishable from a real one at a glance,
 * and a developer who trusts one is being misled by the tool.
 */
"use strict";

// Routes carry the section they belong to. A flat list of fourteen entries is
// a list to read; grouped, it is a place to look — which is the whole reason
// the console this mirrors groups its products rather than listing them.
// Routes carry the product being emulated, grouped the way the Google Cloud
// console groups its products.
//
// The point of this console is that an application cannot tell the difference,
// so the navigation names what is being emulated — Cloud Run, not "Services";
// Pub/Sub, not "Topics" — and groups it under the console's own headings.
// Calling Cloud Run "Serverless" described a category instead of naming the
// product a developer is actually pointing their SDK at.
//
// The page title is the product; what the table holds is the listing's noun.
const ROUTES = [
  { path: "/", service: null, title: "Dashboard" },

  { path: "/run", service: "run", title: "Cloud Run", section: "Serverless computing" },

  // Kubernetes Engine is one product with five pages, not five products.
  //
  // The drawer lists products; a bare row called "Services" or "Jobs" sitting
  // as a peer of Cloud Run is ambiguous with Cloud Run's own services and
  // jobs, and someone who pinned "Jobs" could not tell from the drawer which
  // product they had pinned. `product` is the drawer's row, `title` is the
  // page inside it.
  { path: "/kubernetes/workloads",  service: "workloads",   title: "Workloads", section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/pods",       service: "pods",        title: "Pods",      section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/services",   service: "k8sservices", title: "Services",  section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/jobs",       service: "jobs",        title: "Jobs",      section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/nodes",      service: "nodes",       title: "Nodes",     section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/storage",    service: "k8sstorage",  title: "Storage",   section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },
  { path: "/kubernetes/events",     service: "events",      title: "Events",    section: "Containers",
    product: "kubernetes", productTitle: "Kubernetes Engine" },

  { path: "/storage/browser", service: "storage", title: "Cloud Storage", section: "Storage" },

  { path: "/firestore", service: "firestore", title: "Firestore", section: "Databases" },
  { path: "/datastore", service: "datastore", title: "Datastore", section: "Databases" },
  { path: "/bigtable",  service: "bigtable",  title: "Bigtable",  section: "Databases" },
  { path: "/spanner",   service: "spanner",   title: "Spanner",   section: "Databases" },
  { path: "/cloudsql",  service: "cloudsql",  title: "Cloud SQL", section: "Databases" },

  { path: "/pubsub/topics", service: "pubsub", title: "Pub/Sub",     section: "Integration services" },
  { path: "/tasks/queues",  service: "tasks",  title: "Cloud Tasks", section: "Integration services" },

  // Vertex AI is likewise one product with two pages.
  { path: "/ai/models",     service: "ai",         title: "Model Garden",
    section: "AI and machine learning", product: "vertexai", productTitle: "Vertex AI" },
  { path: "/ai/playground", service: "playground", screen: "playground",
    title: "Studio",
    section: "AI and machine learning", product: "vertexai", productTitle: "Vertex AI" },

  { path: "/secrets", service: "secrets", title: "Secret Manager", section: "Security and identity" },

  { path: "/monitoring", service: null, screen: "monitoring", title: "Monitoring", section: "Operations" },
  { path: "/logs",     service: null, screen: "logs",     title: "Logs Explorer", section: "Operations" },
  { path: "/activity", service: null, screen: "activity", title: "Activity",      section: "Operations" },

  { path: "/projects", service: "projects", title: "Resource Manager", section: "Management tools" },

  { path: "/search", service: null, screen: "search", title: "Search results" },
  { path: "/products", service: null, screen: "products", title: "All products" },
];

// Every product listing gets a matching create address.
//
// A dialog has no URL, so a form opened in one is lost to a refresh, a back
// button or a link sent to a colleague. Whether a given product's form opens
// here or in a dialog is the backend's call — capabilities carry `page` — but
// the address exists either way, so the decision can change without breaking
// a link. Generated rather than written out, because a create path that had
// to be remembered would be forgotten by the next product added.
//
// They carry no `section`, which is what keeps them out of the navigation.
for (const listing of [...ROUTES]) {
  if (!listing.service || listing.screen) continue;
  ROUTES.push({
    path: `${listing.path}/create`, service: listing.service, screen: "create",
    title: `Create in ${listing.title}`, of: listing,
  });
}

// A route's product identity. A single-page product is its own product, so
// every caller can treat the two the same.
const productKey = (entry) => entry.product || entry.service || entry.path;
const productTitle = (entry) => entry.productTitle || entry.title;

// pagesOf returns the pages a product owns, in declaration order.
const pagesOf = (key) => ROUTES.filter((r) => r.section && productKey(r) === key);

// Screens that ship Google's own published product icon, in assets/icons.
//
// The console shows the real mark for the real product, because the point of
// this console is that an application cannot tell the difference and the
// navigation should not make a developer guess which API a screen serves.
// Provenance and terms are recorded in assets/icons/PROVENANCE.md.
//
// The line drawings below remain the fallback for screens with no published
// product icon — the dashboard and search — and for a build where the files
// are missing.
const PRODUCT_ICONS = new Set([
  "run", "storage", "pubsub", "tasks", "secrets", "projects",
  "ai", "playground", "workloads", "pods", "k8sservices", "jobs", "events",
  "logs", "activity",
  "firestore", "datastore", "bigtable", "spanner", "cloudsql",
]);

// Fallback marks, drawn here rather than shipped as files.
const ICONS = {
  // Cloud Run: a container with a run triangle.
  run:       '<rect x="3" y="5" width="18" height="14" rx="3"/><path d="M10 9.5l5 2.5-5 2.5z"/>',
  // Cloud Storage: a bucket.
  storage:   '<path d="M4 7h16l-1.6 11.2a2 2 0 0 1-2 1.8H7.6a2 2 0 0 1-2-1.8z"/><path d="M3 7h18"/><path d="M9 4h6l1 3H8z"/>',
  // Pub/Sub: one publisher, many subscribers.
  pubsub:    '<circle cx="5" cy="12" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="19" cy="12" r="2"/><circle cx="19" cy="18" r="2"/><path d="M7 12h3M10 12l7-5M10 12h7M10 12l7 5"/>',
  // Cloud Tasks: a queue of work, oldest first.
  tasks:     '<rect x="3" y="4" width="18" height="4" rx="1"/><rect x="3" y="10" width="18" height="4" rx="1"/><rect x="3" y="16" width="12" height="4" rx="1"/>',
  // Secret Manager: a key.
  secrets:   '<circle cx="8" cy="12" r="3.5"/><path d="M11.5 12H21"/><path d="M18 12v3M15 12v2.5"/>',
  // Kubernetes Engine: the helm.
  k8s:       '<path d="M12 3l7.5 3.8v10.4L12 21l-7.5-3.8V6.8z"/><circle cx="12" cy="12" r="2.5"/><path d="M12 3v6.5M19.5 6.8l-5.4 4M19.5 17.2l-5.4-4M12 21v-6.5M4.5 17.2l5.4-4M4.5 6.8l5.4 4"/>',
  workloads: '<rect x="3" y="4" width="7" height="7" rx="1"/><rect x="14" y="4" width="7" height="7" rx="1"/><rect x="3" y="13" width="7" height="7" rx="1"/><rect x="14" y="13" width="7" height="7" rx="1"/>',
  pods:      '<path d="M12 3l7.5 3.8v10.4L12 21l-7.5-3.8V6.8z"/>',
  k8sservices: '<circle cx="12" cy="12" r="2.5"/><circle cx="12" cy="4" r="1.8"/><circle cx="19" cy="16" r="1.8"/><circle cx="5" cy="16" r="1.8"/><path d="M12 6v3.5M13.8 13.4l3.6 1.8M10.2 13.4l-3.6 1.8"/>',
  jobs:      '<rect x="3" y="7" width="18" height="13" rx="2"/><path d="M9 7V5a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v2"/><path d="M9 13h6"/>',
  // Cluster nodes: the machines underneath.
  nodes:     '<rect x="3" y="4" width="18" height="6" rx="1"/><rect x="3" y="14" width="18" height="6" rx="1"/><path d="M7 7h.01M7 17h.01"/>',
  // Cluster storage: a stack of disks.
  k8sstorage:'<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  events:    '<circle cx="12" cy="12" r="9"/><path d="M12 7v6M12 16h.01"/>',
  // Vertex AI: a spark.
  ai:        '<path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/><path d="M18 16l.8 2.2L21 19l-2.2.8L18 22l-.8-2.2L15 19l2.2-.8z"/>',
  playground:'<rect x="3" y="4" width="18" height="14" rx="3"/><path d="M8 10.5h.01M12 10.5h.01M16 10.5h.01"/><path d="M8 14h6"/>',
  // Resource Manager: a folder of projects.
  projects:  '<path d="M3 7h6l2 2h10v10H3z"/><path d="M3 7V5h6l2 2"/>',
  logs:      '<path d="M5 4h11l3 3v13H5z"/><path d="M8 11h8M8 15h5"/>',
  activity:  '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  dashboard: '<rect x="3" y="3" width="8" height="10" rx="1"/><rect x="13" y="3" width="8" height="6" rx="1"/><rect x="3" y="15" width="8" height="6" rx="1"/><rect x="13" y="11" width="8" height="10" rx="1"/>',
};

// Category icons, keyed by the section name. Google publishes these beside the
// product icons and they are what the real navigation heads its groups with.
const CATEGORY_ICONS = {
  "Serverless computing": "serverless",
  "Containers": "containers",
  "Storage": "storage",
  "Databases": "databases",
  "Integration services": "integration",
  "AI and machine learning": "ai",
  "Security and identity": "security",
  "Operations": "operations",
  "Management tools": "management",
};

// Capabilities come from the backend, so a control only ever appears when
// the service behind it can actually perform it.
let SERVICES = [];
let DEFAULT_PROJECT = "";
const capabilityOf = (id) => SERVICES.find((s) => s.id === id) || {};

const el = (tag, attrs = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k === "html") node.innerHTML = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined) continue;
    node.append(c);
  }
  return node;
};

const announce = (msg) => { document.getElementById("live").textContent = msg; };

// setChildren replaces a node's children, dropping absent ones.
//
// Node.replaceChildren stringifies anything that is not a Node, so passing a
// conditional child that evaluated to null renders the literal text "null" on
// the page. That is not hypothetical: every list screen without a note showed
// "null" above its filter row. el() already filters its children this way;
// this is the same rule for the places that call replaceChildren directly.
const setChildren = (node, ...children) => {
  node.replaceChildren(...children.flat().filter((c) => c !== null && c !== undefined && c !== false));
};

// --- data ------------------------------------------------------------

// Every call is bounded.
//
// The server bounds its own work, but a request that never reaches it — a
// wedged tunnel, a port-forward that died — would leave the screen loading
// with no elapsed time, no cancel and no eventual error. That is the one
// failure this console is not allowed to have, because it is the one that
// looks exactly like success not having happened yet.
//
// Reads get 30 seconds against the server's own 20s budget; mutations get 70
// against its 60. Both are slack on top of the server's number rather than a
// second opinion about how long the work should take.
const READ_DEADLINE_MS = 30000;
const WRITE_DEADLINE_MS = 70000;

async function api(path, options = {}) {
  const deadline = options.deadline || READ_DEADLINE_MS;
  const controller = new AbortController();
  const started = Date.now();
  const timer = setTimeout(() => controller.abort(), deadline);
  // A caller's own signal is honoured as well as the deadline, so a screen
  // can offer Cancel without having to reimplement the timeout.
  let cancelled = false;
  const outer = options.signal;
  const onOuter = () => { cancelled = true; controller.abort(); };
  if (outer) {
    if (outer.aborted) onOuter();
    else outer.addEventListener("abort", onOuter, { once: true });
  }

  let res;
  try {
    res = await fetch(path, {
      ...options,
      signal: controller.signal,
      headers: { Accept: "application/json", ...(options.headers || {}) },
    });
  } catch (err) {
    // A named path and an elapsed time, because "Failed to fetch" tells the
    // user nothing about which part of their instance stopped answering.
    if (cancelled) throw new Error("cancelled");
    if (controller.signal.aborted) {
      throw new Error(`${path} did not answer within ${Math.round((Date.now() - started) / 1000)}s`);
    }
    throw err;
  } finally {
    clearTimeout(timer);
    if (outer) outer.removeEventListener("abort", onOuter);
  }

  const text = await res.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch { /* not JSON */ }
  if (!res.ok) {
    // The service's own message reaches the screen. A status code alone
    // hides the constraint the caller actually violated.
    const failure = new Error(body.error || `${path} responded ${res.status} ${res.statusText}`);
    // A failed mutation is a record on the server like a successful one, and
    // its id is what lets the panel show one entry rather than two.
    if (body.operation) failure.operation = body.operation;
    throw failure;
  }
  return body;
}

const send = (path, method, body) =>
  api(path, {
    method,
    deadline: WRITE_DEADLINE_MS,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });

// --- failure surfaces --------------------------------------------------
//
// A thrown exception and a hung backend look identical from the user's
// chair, and both look like loading. Nothing caught either: a render function
// that threw left whatever was last painted — usually a skeleton — on screen
// for good, saying nothing.

// screenFailed replaces the current screen with an error card.
function screenFailed(err, what = "This screen") {
  const view = document.getElementById("view");
  const message = err && err.message ? err.message : String(err);
  if (!view) return;
  // Not errorState: its fixed line says the console could not read something
  // from the instance, and this is the console's own code failing. Saying the
  // backend broke would send the user to debug the wrong thing.
  setChildren(view, el("div", { class: "state error", role: "alert" },
    el("h2", { text: `${what} stopped` }),
    el("p", { text: "The console hit an error in its own code. This is a bug in CloudBurrow." }),
    el("pre", { text: message }),
    el("button", { class: "secondary", text: "Reload", onclick: () => location.reload() })));
  announce(`${what} stopped: ${message}`);
}

// shellFailed is for a failure with no screen to replace — one raised before
// or outside a render.
function shellFailed(err) {
  const message = err && err.message ? err.message : String(err);
  let banner = document.getElementById("shell-error");
  if (!banner) {
    banner = el("div", { id: "shell-error", class: "shell-error", role: "alert" });
    document.body.prepend(banner);
  }
  setChildren(banner,
    el("span", { text: `The console hit an error: ${message}` }),
    el("button", { class: "secondary", text: "Reload", onclick: () => location.reload() }));
}

function installFailureSurfaces() {
  window.addEventListener("error", (e) => {
    // A failed resource load fires this too, with no Error object; those are
    // not script failures and must not blank the page.
    if (!e.error) return;
    shellFailed(e.error);
  });
  window.addEventListener("unhandledrejection", (e) => shellFailed(e.reason));
}

// --- operations ------------------------------------------------------
//
// One ledger, two views.
//
// The server holds the record — /api/operations — and the notifications panel
// is a view of it rather than a second copy of it. There used to be two
// independent ledgers: the panel was fed by an in-memory array that nothing
// seeded, so reloading the page silently erased the record of what had just
// happened while the same operations were still sitting in the Activity
// screen. The two surfaces could flatly contradict each other.
//
// The in-memory entries are optimistic and exist only between the click and
// the response — the one window the server cannot see. As soon as a response
// names its operation id, the local entry is reconciled away.

const OPERATIONS = [];
let SERVER_OPERATIONS = [];
// Keys the user has already looked at. Seeded from whatever exists at first
// load, so the badge counts what has happened since the console was opened
// rather than announcing the whole history on every reload.
const SEEN_OPERATIONS = new Set();
let LOCAL_SEQ = 0;

function recordOperation(label) {
  const op = { key: `local-${++LOCAL_SEQ}`, label, state: "running", at: new Date() };
  OPERATIONS.unshift(op);
  renderOperations();

  const finish = (state) => (detail, id) => {
    op.state = state;
    op.detail = detail || "";
    if (id) { op.id = id; op.key = id; }
    renderOperations();
    // The server's own record is the one that survives a reload, so it is
    // fetched as soon as there is something new in it.
    refreshOperations();
  };
  return { succeeded: finish("succeeded"), failed: finish("failed") };
}

// relativeTime answers the question the panel is actually asked.
//
// "Which of these happened first, and how long ago" is the reading; a
// wall-clock time makes the reader do the subtraction, and the timestamp was
// being captured and then thrown away entirely.
function relativeTime(date) {
  const secs = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (secs < 10) return "just now";
  if (secs < 60) return `${secs} sec ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} hr ago`;
  return `${Math.round(hours / 24)} d ago`;
}

const OPERATION_STATES = { SUCCEEDED: "succeeded", FAILED: "failed" };

// mergedOperations is the panel's list: the server's record, plus the local
// entries it does not know about yet.
function mergedOperations() {
  const fromServer = SERVER_OPERATIONS.map((o) => ({
    key: o.id, id: o.id, title: `${o.kind} ${o.resource || ""}`.trim(),
    state: OPERATION_STATES[o.state] || "running",
    detail: o.error || "", at: new Date(o.started),
  }));
  const known = new Set(fromServer.map((o) => o.id));
  const local = OPERATIONS
    .filter((o) => !o.id || !known.has(o.id))
    .map((o) => ({ key: o.key, id: o.id, title: o.label, state: o.state,
                   detail: o.detail || "", at: o.at }));
  return [...local, ...fromServer].sort((a, b) => b.at - a.at).slice(0, 20);
}

async function refreshOperations() {
  try {
    const data = await api(`/api/operations?project=${encodeURIComponent(currentProject())}`);
    SERVER_OPERATIONS = data.operations || [];
    OPERATIONS_STALE = "";
  } catch (err) {
    // Reported in the panel rather than as a snackbar: the bell failing to
    // refresh is worth knowing when you look at it, and not worth
    // interrupting whatever you were doing.
    OPERATIONS_STALE = err.message;
  }
  renderOperations();
}

let OPERATIONS_STALE = "";

function renderOperations() {
  const list = document.getElementById("notification-list");
  const count = document.getElementById("notification-count");
  const empty = document.querySelector("#notifications-panel .panel-empty");
  const stale = document.getElementById("notification-stale");
  if (!list) return;

  const ops = mergedOperations();

  setChildren(list, ...ops.map((op) =>
    el("li", { class: SEEN_OPERATIONS.has(op.key) ? null : "is-unread" },
      // Running is not a warning. Rendering it in the same amber the tables
      // use for "Paused" said something had gone slightly wrong; a moving
      // indicator says the only true thing, which is that it is still going.
      op.state === "running"
        ? el("span", { class: "status is-working" },
            spinner(), el("span", { text: op.title }))
        : el("span", { class: "status", "data-state": op.state === "succeeded" ? "ok" : "error" },
            el("span", { text: op.title })),
      el("div", { class: "op-when unavailable", text: relativeTime(op.at) }),
      // A failure carries its cause, and the cause links to the lines that
      // produced it: "it failed" without the reason is the least useful thing
      // a console can say.
      op.detail
        ? (op.state === "failed" && op.id
            ? el("a", { class: "op-detail",
                        href: `/logs?operation=${encodeURIComponent(op.id)}`,
                        text: op.detail })
            : el("div", { class: "op-detail unavailable", text: op.detail }))
        : null)));

  if (empty) empty.hidden = ops.length > 0;
  if (stale) {
    stale.hidden = !OPERATIONS_STALE;
    stale.textContent = OPERATIONS_STALE
      ? `Showing the last known list — refresh failed: ${OPERATIONS_STALE}`
      : "";
  }

  // The badge counts what has not been looked at, not what is running. A
  // badge that cleared itself the moment an operation finished told the user
  // nothing about the failure they had not seen yet.
  const unread = ops.filter((o) => !SEEN_OPERATIONS.has(o.key)).length;
  if (unread > 0) { count.hidden = false; count.textContent = String(unread); }
  else { count.hidden = true; }

  const bar = document.getElementById("busy-bar");
  if (bar) bar.hidden = !ops.some((o) => o.state === "running");
}

// markOperationsSeen is what opening the panel means.
function markOperationsSeen() {
  for (const op of mergedOperations()) SEEN_OPERATIONS.add(op.key);
  renderOperations();
}

// --- theme -----------------------------------------------------------
//
// The documented console behaviour is that changing the theme does not
// reload the page, so the attribute is swapped in place.

function applyTheme(choice) {
  const root = document.documentElement;
  if (choice === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", choice);
  try { localStorage.setItem("cb-theme", choice); } catch { /* private mode */ }
}

function initTheme() {
  let stored = "system";
  try { stored = localStorage.getItem("cb-theme") || "system"; } catch { /* private mode */ }
  applyTheme(stored);
  for (const input of document.querySelectorAll('input[name="theme"]')) {
    input.checked = input.value === stored;
    input.addEventListener("change", () => applyTheme(input.value));
  }
}

// --- panels ----------------------------------------------------------

// --- overlays ---------------------------------------------------------
//
// An overlay that appears between two frames gives no sense of where it came
// from. `hidden` on its own cannot be animated — display:none has no
// intermediate state — so the attribute comes off a frame before the open
// class goes on, and goes back on once the exit has run.
//
// The class, not the attribute, is the source of truth for "is this open":
// during the exit the panel is still in the layout and must not be treated as
// open by the toggle or the outside-click handler.
const OVERLAY_EXIT_MS = 200;
const OVERLAY_TIMERS = new WeakMap();

const overlayOpen = (node) => node.classList.contains("is-open");

function showOverlay(node) {
  const pending = OVERLAY_TIMERS.get(node);
  if (pending) { clearTimeout(pending); OVERLAY_TIMERS.delete(node); }
  node.hidden = false;
  requestAnimationFrame(() => node.classList.add("is-open"));
}

function hideOverlay(node) {
  node.classList.remove("is-open");
  const pending = OVERLAY_TIMERS.get(node);
  if (pending) clearTimeout(pending);
  // Hidden after the exit, so the panel does not vanish mid-animation. With
  // motion reduced the transition is none and this is simply a short delay
  // before an already-invisible node is taken out of the tree.
  OVERLAY_TIMERS.set(node, setTimeout(() => {
    node.hidden = true;
    OVERLAY_TIMERS.delete(node);
  }, OVERLAY_EXIT_MS));
}

// The search field collapses to a control at narrow widths rather than
// disappearing.
//
// It used to be `display: none` below 960px, which deleted the console's most
// prominent control on the width most likely to be a phone — and offered
// nothing in its place, so cross-service search was simply unavailable there.
function initSearchToggle() {
  const toggle = document.getElementById("search-toggle");
  const input = document.getElementById("search");
  if (!toggle || !input) return;

  const root = document.documentElement;
  const close = () => {
    root.removeAttribute("data-search");
    toggle.setAttribute("aria-expanded", "false");
  };

  toggle.addEventListener("click", () => {
    if (root.getAttribute("data-search") === "open") { close(); toggle.focus(); return; }
    root.setAttribute("data-search", "open");
    toggle.setAttribute("aria-expanded", "true");
    input.focus();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { close(); toggle.focus(); }
  });
  // Leaving the field puts the bar back, the way a collapsed search does
  // everywhere else. Deferred, because the blur fires before a click on
  // anything inside the field's own row.
  input.addEventListener("blur", () => setTimeout(() => {
    if (document.activeElement !== input) close();
  }, 100));
}

function initPanel(buttonId, panelId, opts = {}) {
  const button = document.getElementById(buttonId);
  const panel = document.getElementById(panelId);

  const close = () => {
    hideOverlay(panel);
    button.setAttribute("aria-expanded", "false");
  };
  const open = () => {
    showOverlay(panel);
    button.setAttribute("aria-expanded", "true");
    if (opts.onOpen) opts.onOpen();
    // Focus moves into the dialog, and Escape returns it: a dialog that
    // traps neither is one a keyboard user cannot leave.
    const first = panel.querySelector("input, button, a, select");
    if (first) first.focus();
  };

  button.addEventListener("click", () => (overlayOpen(panel) ? (close(), button.focus()) : open()));
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { close(); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    if (overlayOpen(panel) && !panel.contains(e.target) && !button.contains(e.target)) close();
  });
}

// --- navigation ------------------------------------------------------

// The navigation menu.
//
// Modelled on the console this mirrors: a pinned section at the top, then the
// remaining products grouped under Google's own product categories, each group
// collapsible. Products can be pinned and unpinned, and the whole menu can be
// pinned open so it stops overlaying the page.
//
// Category names and their icons are Google's published taxonomy, taken from
// the category-icons set they publish beside the product icons — not names
// invented here.

const PINNED_KEY = "cloudburrow.pinned";
const OPEN_GROUPS_KEY = "cloudburrow.navgroups";
const MORE_OPEN_KEY = "cloudburrow.navmore";

function readStored(key, fallback) {
  try {
    const raw = localStorage.getItem(key);
    return raw ? JSON.parse(raw) : fallback;
  } catch { return fallback; }
}

function writeStored(key, value) {
  try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* private mode */ }
}

// Pinned by default: the services a developer opens first. The console ships
// a default pin set too rather than an empty menu.
const DEFAULT_PINNED = ["storage", "pubsub", "run"];

// An ordered list rather than a set: the menu this mirrors lets you arrange
// your pinned products, and a set cannot hold an order.
let PINNED = null;
let OPEN_GROUPS = null;
// Set while the user is rearranging the menu, so a reveal does not fight the
// control they just used. Cleared on navigation, where revealing is the point.
let REVEAL_SUSPENDED = false;

function pinnedOrder() {
  if (!PINNED) PINNED = readStored(PINNED_KEY, DEFAULT_PINNED).slice();
  return PINNED;
}

function isPinned(service) {
  return pinnedOrder().includes(service);
}

function togglePinned(service) {
  const order = pinnedOrder();
  const at = order.indexOf(service);
  if (at >= 0) order.splice(at, 1);
  else order.push(service);
  writeStored(PINNED_KEY, order);
}

// movePinned reorders one product, which is what dragging does.
function movePinned(service, before) {
  const order = pinnedOrder();
  const from = order.indexOf(service);
  if (from < 0) return;
  order.splice(from, 1);
  const to = before ? order.indexOf(before) : order.length;
  order.splice(to < 0 ? order.length : to, 0, service);
  writeStored(PINNED_KEY, order);
}

function openGroups() {
  if (!OPEN_GROUPS) OPEN_GROUPS = new Set(readStored(OPEN_GROUPS_KEY, []));
  return OPEN_GROUPS;
}

function markFor(entry) {
  return PRODUCT_ICONS.has(entry.service)
    ? el("img", { class: "nav-icon-img", src: `/icons/${entry.service}.svg`, alt: "",
                  width: "20", height: "20", loading: "lazy" })
    : el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${ICONS[entry.service] || ICONS.dashboard}</svg>` });
}

// navLink renders one product row, with its pin control.
// dragHandle makes a pinned row movable, as the real menu's are.
function dragHandle(entry, redraw) {
  return el("span", {
    class: "nav-drag", "aria-hidden": "true", title: "Drag to reorder",
    html: '<svg viewBox="0 0 24 24"><path d="M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01"/></svg>',
  });
}

// --- the category flyout -----------------------------------------------
//
// Hovering or focusing a category opens a panel beside the drawer listing its
// products. The drawer's own scroll position never moves, which is the point:
// expanding in place pushes the row you pointed at out of view.

let FLYOUT = null;
let FLYOUT_OPEN_TIMER = null;
let FLYOUT_CLOSE_TIMER = null;
// Returning focus to the category that opened the panel would fire its own
// focus handler and open the panel straight back up, so Escape could never
// dismiss it. Set for the duration of that one focus() call.
let FLYOUT_REFUSE_FOCUS = false;
const FLYOUT_INTENT_MS = 180;

function flyoutHost() {
  if (FLYOUT) return FLYOUT;
  FLYOUT = el("div", { id: "nav-flyout", class: "nav-flyout", hidden: true, role: "menu" });
  FLYOUT.addEventListener("mouseenter", () => {
    if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  });
  FLYOUT.addEventListener("mouseleave", () => scheduleFlyoutClose());
  FLYOUT.addEventListener("keydown", (e) => {
    const items = [...FLYOUT.querySelectorAll("a")];
    const at = items.indexOf(document.activeElement);
    if (e.key === "Escape" || e.key === "ArrowLeft") {
      e.preventDefault();
      // Escape dismisses the panel, not the whole drawer. Without this the
      // global handler closed the menu underneath it and took focus to the
      // hamburger, so one key did two things and neither was undoable.
      e.stopPropagation();
      const opener = FLYOUT.opener;
      closeFlyout();
      if (opener && opener.isConnected) {
        FLYOUT_REFUSE_FOCUS = true;
        opener.focus();
        FLYOUT_REFUSE_FOCUS = false;
      }
      return;
    }
    const step = e.key === "ArrowDown" ? 1 : e.key === "ArrowUp" ? -1 : 0;
    if (!step || !items.length) return;
    e.preventDefault();
    items[(Math.max(at, 0) + step + items.length) % items.length].focus();
  });
  document.body.append(FLYOUT);
  return FLYOUT;
}

function scheduleFlyoutClose() {
  if (FLYOUT_CLOSE_TIMER) clearTimeout(FLYOUT_CLOSE_TIMER);
  FLYOUT_CLOSE_TIMER = setTimeout(closeFlyout, FLYOUT_INTENT_MS);
}

function closeFlyout() {
  if (FLYOUT_OPEN_TIMER) { clearTimeout(FLYOUT_OPEN_TIMER); FLYOUT_OPEN_TIMER = null; }
  if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  if (!FLYOUT) return;
  hideOverlay(FLYOUT);
  if (FLYOUT.opener) FLYOUT.opener.setAttribute("aria-expanded", "false");
}

function openFlyout(toggle, section, items, redraw, focusFirst = false) {
  // Below the docked width there is no room beside the drawer, and a touch
  // device has no hover to express intent with: there, clicking to expand is
  // the whole interaction.
  if (FLYOUT_REFUSE_FOCUS) return;
  // A closed drawer is visibility:hidden, and a panel hanging off an
  // invisible row is a menu with no menu.
  if (document.documentElement.dataset.nav === "closed") return;
  if (!window.matchMedia("(min-width: 1280px) and (hover: hover)").matches) return;
  if (FLYOUT_CLOSE_TIMER) { clearTimeout(FLYOUT_CLOSE_TIMER); FLYOUT_CLOSE_TIMER = null; }
  if (FLYOUT_OPEN_TIMER) clearTimeout(FLYOUT_OPEN_TIMER);

  // A delay, so dragging the pointer down the drawer does not flash a panel
  // for every category it crosses.
  const show = () => {
    const host = flyoutHost();
    host.opener = toggle;
    const box = toggle.getBoundingClientRect();
    host.style.top = `${Math.round(box.top)}px`;
    host.style.left = `${Math.round(box.right + 4)}px`;
    setChildren(host,
      el("p", { class: "nav-flyout-title", text: section }),
      el("ul", {}, ...items.map((entry) =>
        el("li", {},
          el("a", {
            href: entry.path + scopeSearch(), role: "menuitem",
            onclick: () => closeFlyout(),
          },
            el("span", { class: "nav-icon" }, markFor(entry)),
            el("span", { text: productTitle(entry) }))))));
    showOverlay(host);
    toggle.setAttribute("aria-expanded", "true");
    if (focusFirst) {
      const first = host.querySelector("a");
      if (first) first.focus();
    }
  };
  if (focusFirst) show();
  else FLYOUT_OPEN_TIMER = setTimeout(show, FLYOUT_INTENT_MS);
}

function navLink(entry, onPinChange, nested = false, draggable = false) {
  // The drawer names and pins the product, so pinning "Kubernetes Engine"
  // cannot be mistaken for pinning one of its five pages.
  const key = productKey(entry);
  const label = entry.section ? productTitle(entry) : entry.title;
  const pinned = isPinned(key);
  const pin = el("button", {
    class: "nav-pin" + (pinned ? " is-pinned" : ""),
    "aria-label": (pinned ? "Unpin " : "Pin ") + label,
    "aria-pressed": pinned ? "true" : "false",
    title: pinned ? "Unpin" : "Pin",
    onclick: (e) => {
      e.preventDefault();
      e.stopPropagation();
      togglePinned(key);
      onPinChange();
    },
  }, el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${PIN_ICON}</svg>` }));

  const row = el("li", {
    class: "nav-item" + (nested ? " nav-nested" : "") + (draggable ? " is-draggable" : ""),
    draggable: draggable ? "true" : null,
    "data-service": key || null,
  },
    draggable ? dragHandle(entry, onPinChange) : null,
    el("a", {
      href: entry.path + scopeSearch(),
      // Every page the product owns, so the drawer marks the product while
      // any of its pages is open.
      "data-path": pagesOf(key).map((r) => r.path).join(" ") || entry.path,
      "aria-label": label, title: label,
    },
      el("span", { class: "nav-icon" }, markFor(entry)),
      el("span", { class: "nav-label", text: label })),
    entry.service ? pin : null);

  if (draggable) {
    row.addEventListener("dragstart", (e) => {
      e.dataTransfer.setData("text/plain", key);
      e.dataTransfer.effectAllowed = "move";
      row.classList.add("is-dragging");
    });
    row.addEventListener("dragend", () => row.classList.remove("is-dragging"));
    row.addEventListener("dragover", (e) => {
      e.preventDefault();
      e.dataTransfer.dropEffect = "move";
      row.classList.add("is-drop-target");
    });
    row.addEventListener("dragleave", () => row.classList.remove("is-drop-target"));
    row.addEventListener("drop", (e) => {
      e.preventDefault();
      row.classList.remove("is-drop-target");
      const moved = e.dataTransfer.getData("text/plain");
      if (moved && moved !== key) {
        movePinned(moved, key);
        onPinChange();
      }
    });
  }
  return row;
}

// scopeSearch is the query string a navigation link may carry.
//
// Only the project travels. Passing on the whole of location.search took the
// current screen's own state with it — a detail screen's ?resource= made the
// drawer's link to that product re-open the resource you were already looking
// at, and a search's ?q= rode along to every product.
function scopeSearch() {
  const from = new URLSearchParams(location.search);
  const scope = new URLSearchParams();
  for (const key of ["project"]) {
    const value = from.get(key);
    if (value) scope.set(key, value);
  }
  const query = scope.toString();
  return query ? "?" + query : "";
}

const PIN_ICON = '<path d="M9 4h6l-1 6 3 3v2H7v-2l3-3z"/><path d="M12 15v5"/>';

function buildNav(services) {
  const list = document.getElementById("nav-list");
  const available = new Set(services.map((s) => s.id));
  const redraw = () => buildNav(services);

  // Screens, not every address. A create form and the catalogue page both
  // have routes so they can be linked to; neither is a row in the menu.
  const entries = ROUTES.filter((r) =>
    r.path !== "/search" && r.path !== "/products" && r.screen !== "create" &&
    (!r.service || available.has(r.service) || !r.section));

  const dashboard = entries.find((e) => e.path === "/");
  // One row per product, not per page. A product's first available page is
  // what the row points at, and the page it lands on is marked by the
  // in-product navigation rather than by the drawer.
  const pages = entries.filter((e) => e.path !== "/" && e.section);
  const products = [];
  const seenProducts = new Set();
  for (const page of pages) {
    const key = productKey(page);
    if (seenProducts.has(key)) continue;
    seenProducts.add(key);
    products.push(page);
  }
  // In the order the developer arranged them, not the order they are declared.
  const byProduct = new Map(products.map((e) => [productKey(e), e]));
  const pinned = pinnedOrder().map((id) => byProduct.get(id)).filter(Boolean);

  // A rule, not a heading's border: the blocks are separate things and the
  // separator belongs between them rather than attached to whichever heading
  // happens to come next.
  const divider = () => el("li", { class: "nav-divider", role: "separator" });

  const children = [];
  if (dashboard) children.push(navLink(dashboard, redraw));

  // Pinned first, as the console does. The heading is omitted when nothing is
  // pinned rather than leaving an empty section.
  if (pinned.length) {
    if (dashboard) children.push(divider());
    children.push(el("li", { class: "nav-section", role: "presentation" },
      el("span", { text: "Pinned" })));
    children.push(...pinned.map((e) => navLink(e, redraw, false, true)));
  }

  // The break between the pinned products and the full catalogue. Without it
  // the two read as one list and a pinned product looks like a category.
  if (dashboard || pinned.length) children.push(divider());

  // The catalogue sits behind "More products", as it does in the menu this
  // mirrors: pinned products are what you reach for, and everything else is
  // one level further in rather than a wall of categories under them.
  // The category holding the screen you are on is opened for this render only.
  //
  // "Where am I" is the first question a menu answers, and it could not answer
  // it: both the catalogue and every category default to closed, so a deep
  // link to a product left the drawer with no row for the current screen and
  // nothing marked. Revealing it is not a preference change, so it is not
  // written to storage — collapsing the category again must still stick.
  // Matched against every page, not only the row the drawer shows: a deep
  // link to /kubernetes/pods has to reveal Containers even though the row
  // there is Kubernetes Engine.
  const active = pages.find((e) => e.path === location.pathname);
  const revealed = REVEAL_SUSPENDED ? null : (active ? active.section : null);

  // The stored preference and the effective state are kept apart on purpose.
  //
  // A toggle must act on what the user chose, not on what the reveal forced.
  // Computing `!moreOpen` from the effective value left the control stuck: on
  // a revealed screen the effective value is always true, so the toggle could
  // only ever write false, and the catalogue could never be opened for good.
  const moreStored = readStored(MORE_OPEN_KEY, false);
  const moreOpen = moreStored || Boolean(revealed);
  children.push(el("li", { class: "nav-group-item" },
    el("button", {
      class: "nav-group nav-more" + (moreOpen ? " is-open" : ""),
      "aria-expanded": moreOpen ? "true" : "false",
      onclick: () => {
        // Acting on the control drops the reveal, so the choice sticks.
        REVEAL_SUSPENDED = true;
        writeStored(MORE_OPEN_KEY, !moreStored);
        redraw();
      },
    },
      el("span", { class: "nav-label", text: "More products" }),
      el("span", { class: "nav-chevron", "aria-hidden": "true",
                   html: '<svg viewBox="0 0 24 24"><path d="M9 6l6 6-6 6"/></svg>' }))));

  // Then the categories, each collapsible and carrying Google's own icon.
  const groups = new Map();
  for (const e of products) {
    if (!groups.has(e.section)) groups.set(e.section, []);
    groups.get(e.section).push(e);
  }

  for (const [section, items] of moreOpen ? groups : []) {
    const open = openGroups().has(section) || section === revealed;
    const slug = CATEGORY_ICONS[section];
    const toggle = el("button", {
      class: "nav-group" + (open ? " is-open" : ""),
      "aria-expanded": open ? "true" : "false",
      "aria-haspopup": "true",
      // Pointing opens the category beside the drawer; clicking still expands
      // it in place. With nine categories in a 320px column, expanding pushes
      // the row you clicked out of view and makes comparing two of them
      // impossible — the flyout is what makes the catalogue browsable by
      // pointing, and the in-place expansion stays for touch and for narrow
      // windows where there is no room beside the drawer.
      onmouseenter: () => openFlyout(toggle, section, items, redraw),
      onmouseleave: () => scheduleFlyoutClose(),
      onfocus: () => openFlyout(toggle, section, items, redraw),
      onkeydown: (e) => {
        if (e.key === "ArrowRight") { e.preventDefault(); openFlyout(toggle, section, items, redraw, true); }
        if (e.key === "Escape") closeFlyout();
      },
      onclick: () => {
        closeFlyout();
        REVEAL_SUSPENDED = true;
        const set = openGroups();
        if (set.has(section)) set.delete(section);
        else set.add(section);
        writeStored(OPEN_GROUPS_KEY, [...set]);
        redraw();
      },
    },
      slug
        ? el("img", { class: "nav-icon-img", src: `/icons/categories/${slug}.svg`, alt: "",
                      width: "20", height: "20", loading: "lazy" })
        : el("span", { class: "nav-icon" }),
      el("span", { class: "nav-label", text: section }),
      el("span", { class: "nav-chevron", "aria-hidden": "true",
                   html: '<svg viewBox="0 0 24 24"><path d="M9 6l6 6-6 6"/></svg>' }));

    children.push(el("li", { class: "nav-group-item" }, toggle));
    if (open) {
      children.push(...items.map((e) => navLink(e, redraw, true)));
    }
  }

  // Screens with no product category sit at the end, as utilities do, behind
  // their own rule.
  const loose = entries.filter((x) => x.path !== "/" && !x.section);
  if (loose.length) {
    children.push(divider());
    for (const e of loose) children.push(navLink(e, redraw));
  }

  setChildren(list, ...children);
  markCurrent();

  // A revealed row deep in the catalogue is no use below the fold.
  const current = list.querySelector('a[aria-current="page"]');
  if (current && revealed) {
    current.scrollIntoView({ block: "nearest" });
  }
}

function markCurrent() {
  for (const a of document.querySelectorAll("#nav a")) {
    // A drawer row stands for a product, and data-path lists every page that
    // product owns — so the row stays marked wherever inside it you are.
    const paths = (a.dataset.path || "").split(" ");
    if (paths.includes(location.pathname)) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// The navigation menu.
//
// Three states, as the console it mirrors has:
//
//   closed  the menu is not on screen and the page uses the full width
//   open    it overlays the page, above a scrim, and closes when you pick
//           something — which is what a menu does
//   docked  it is pinned open and the page sits beside it
//
// The menu button toggles closed and open. The pin inside the menu docks it.
// Both are remembered, because a menu you have to reopen on every page is a
// menu you stop using.
const NAV_STATE_KEY = "cloudburrow.nav";

function navState() {
  return document.documentElement.dataset.nav || "closed";
}

function navIsNarrow() {
  return window.matchMedia("(max-width: 959px)").matches;
}

function applyNavState(state) {
  const nav = document.getElementById("nav");
  const scrim = document.getElementById("nav-scrim");
  const toggle = document.getElementById("nav-toggle");
  const dock = document.getElementById("nav-dock");

  document.documentElement.dataset.nav = state;
  const open = state === "open" || state === "docked";
  toggle.setAttribute("aria-expanded", open ? "true" : "false");
  dock.setAttribute("aria-pressed", state === "docked" ? "true" : "false");
  dock.setAttribute("aria-label", state === "docked" ? "Unpin menu" : "Keep menu open");
  // Only the overlay is modal. A docked menu is part of the page and must not
  // be hidden from a screen reader or sit behind a scrim.
  scrim.hidden = state !== "open";
  nav.setAttribute("aria-hidden", state === "closed" ? "true" : "false");

  // While the menu is modal the content behind it is not interactive, and it
  // should not be reachable by a screen reader either.
  //
  // Only the content region. The toolbar keeps working, because the menu
  // button lives in it and is one of the ways out — and inert cannot be
  // undone on a descendant, so marking the toolbar would disable the very
  // control that closes the menu. Tab is kept inside the drawer by the trap
  // in initNavToggle rather than by making the toolbar unreachable.
  const main = document.getElementById("main");
  if (main) {
    if (state === "open") main.setAttribute("inert", "");
    else main.removeAttribute("inert");
  }
}

function setNavState(state, { remember = true } = {}) {
  applyNavState(state);
  if (remember && !navIsNarrow()) {
    try { localStorage.setItem(NAV_STATE_KEY, state); } catch { /* private mode */ }
  }
}

function openNav() {
  setNavState("open", { remember: false });
  // Focus moves into the menu, and Escape gives it back: a modal you cannot
  // leave by keyboard is a trap.
  const first = document.querySelector("#nav a, #nav button");
  if (first) first.focus();
}

function closeNav({ focusToggle = false } = {}) {
  setNavState("closed", { remember: false });
  if (focusToggle) document.getElementById("nav-toggle").focus();
}

function initNavToggle() {
  const toggle = document.getElementById("nav-toggle");
  const scrim = document.getElementById("nav-scrim");
  const dock = document.getElementById("nav-dock");
  const nav = document.getElementById("nav");

  // A wide window starts docked and a narrow one closed, which is what the
  // width itself suggests. A remembered choice wins over both.
  let initial = "closed";
  if (!navIsNarrow()) {
    let stored = null;
    try { stored = localStorage.getItem(NAV_STATE_KEY); } catch { /* ignore */ }
    initial = stored === "closed" || stored === "docked" ? stored
      : window.matchMedia("(min-width: 1280px)").matches ? "docked" : "closed";
  }
  applyNavState(initial);

  toggle.addEventListener("click", () => {
    if (navState() === "closed") openNav();
    else if (navState() === "open") closeNav();
    else setNavState("closed"); // undocking through the menu button
  });
  scrim.addEventListener("click", () => closeNav({ focusToggle: true }));

  dock.addEventListener("click", () => {
    setNavState(navState() === "docked" ? "open" : "docked");
    dock.focus();
  });

  document.addEventListener("keydown", (e) => {
    if (navState() !== "open") return;
    if (e.key === "Escape") { closeNav({ focusToggle: true }); return; }
    if (e.key !== "Tab") return;

    // Focus stays in the menu while it is modal. Without this, tabbing walks
    // out of the drawer and onto the page behind the scrim — controls the user
    // cannot see and, because the scrim is covering them, cannot click either.
    const focusable = [...nav.querySelectorAll("a[href], button:not([disabled])")]
      .filter((el) => el.offsetParent !== null);
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });

  // Following a link closes an overlaid menu. A docked one stays, because it
  // is not covering anything.
  nav.addEventListener("click", (e) => {
    if (navState() === "open" && e.target.closest("a")) closeNav();
  });

  document.getElementById("nav-all").addEventListener("click", () => {
    // It navigates, and it leaves the drawer alone.
    //
    // It used to expand all nine categories in place and write that to
    // storage — producing one scrolling column of twenty rows that is harder
    // to scan than the collapsed state it replaced, and permanently
    // overwriting a preference the user had set by hand, with nothing to
    // restore it. It was the only control in this console that did that.
    closeNav();
    navigate("/products");
  });
}

// renderProducts is the full catalogue, in the content area.
//
// Twenty products across nine categories do not fit a 320px drawer; the
// console this mirrors puts them on a page laid out in columns, and so does
// this. Pinning from here writes the same store the drawer reads.
async function renderProducts(view) {
  const available = new Set(SERVICES.map((sv) => sv.id));
  const entries = ROUTES.filter((r) =>
    r.section && (!r.service || available.has(r.service)));

  // One card per product, not per page, matching the drawer.
  const products = [];
  const seen = new Set();
  for (const page of entries) {
    const key = productKey(page);
    if (seen.has(key)) continue;
    seen.add(key);
    products.push(page);
  }

  const groups = new Map();
  for (const entry of products) {
    if (!groups.has(entry.section)) groups.set(entry.section, []);
    groups.get(entry.section).push(entry);
  }

  const filter = el("input", {
    class: "filter", type: "search", placeholder: "Filter products",
    "aria-label": "Filter products",
  });
  const grid = el("div", { class: "catalogue" });

  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    const matches = (entry) =>
      !q || productTitle(entry).toLowerCase().includes(q) ||
      entry.section.toLowerCase().includes(q) ||
      pagesOf(productKey(entry)).some((page) => page.title.toLowerCase().includes(q));

    const shown = [...groups]
      .map(([section, items]) => [section, items.filter(matches)])
      .filter(([, items]) => items.length);

    if (!shown.length) {
      return setChildren(grid,
        el("div", { class: "state state-inline" },
          el("h2", { text: "No matching products" }),
          el("p", { text: `Nothing matches “${q}”.` }),
          el("button", { class: "secondary", text: "Clear filter",
                         onclick: () => { filter.value = ""; draw(); } })));
    }

    setChildren(grid, ...shown.map(([section, items]) =>
      el("section", { class: "catalogue-group" },
        el("h2", {},
          CATEGORY_ICONS[section]
            ? el("img", { class: "nav-icon-img", src: `/icons/categories/${CATEGORY_ICONS[section]}.svg`,
                          alt: "", width: "20", height: "20", loading: "lazy" })
            : null,
          el("span", { text: section })),
        el("ul", {}, ...items.map((entry) => {
          const key = productKey(entry);
          const pinned = isPinned(key);
          return el("li", { class: "catalogue-item" },
            el("a", { href: entry.path + scopeSearch() },
              el("span", { class: "nav-icon" }, markFor(entry)),
              el("span", {}, el("span", { class: "catalogue-name", text: productTitle(entry) }),
                pagesOf(key).length > 1
                  ? el("span", { class: "catalogue-pages",
                                 text: pagesOf(key).map((pg) => pg.title).join(" · ") })
                  : null)),
            el("button", {
              class: "nav-pin" + (pinned ? " is-pinned" : ""),
              "aria-label": (pinned ? "Unpin " : "Pin ") + productTitle(entry),
              "aria-pressed": pinned ? "true" : "false",
              title: pinned ? "Unpin" : "Pin",
              onclick: () => { togglePinned(key); buildNav(SERVICES); draw(); },
            }, el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${PIN_ICON}</svg>` })));
        })))));
  };

  filter.addEventListener("input", draw);
  draw();

  setChildren(view,
    pageHeader("All products", "Every product this instance emulates. Pin one to keep it at the top of the menu."),
    el("div", { class: "filter-bar" }, filter),
    grid);
  announce(`${products.length} products`);
}

// --- states ----------------------------------------------------------
//
// Four distinct states, because each is separately easy to get wrong and an
// error rendered as an empty table is the one that costs a developer an hour.

// A skeleton that eventually says something.
//
// Left alone it shimmers forever, and a shimmer is indistinguishable from
// progress. After a few seconds it names what it is waiting for and offers a
// way out, which is the escalation the console this mirrors performs.
const STILL_LOADING_MS = 8000;

const loadingState = (rows = 5, opts = {}) => {
  const skeleton = el("div", { class: "skeleton", "aria-label": "Loading", role: "status" },
    Array.from({ length: rows }, () => el("div")));
  if (!opts.what) return skeleton;

  const note = el("p", { class: "still-loading", hidden: true, role: "status" },
    el("span", { text: `Still loading ${opts.what}…` }),
    opts.onCancel
      ? el("button", { class: "secondary", text: "Cancel", onclick: opts.onCancel })
      : null);
  const wrap = el("div", { class: "loading" }, skeleton, note);
  // Checked rather than cleared: the skeleton is replaced wholesale when the
  // data arrives, so there is nothing left to hold a handle on.
  setTimeout(() => { if (wrap.isConnected) note.hidden = false; }, STILL_LOADING_MS);
  return wrap;
};

const emptyState = (title, hint) =>
  el("div", { class: "state" },
    el("h2", { text: title }),
    el("p", { text: hint }));

// pageHeader is the block every product screen opens with.
//
// Three screens emitted a bare h1 instead, so the title sat at a different
// height with no rule under it and no room for a subtitle — the kind of drift
// that makes one console feel like three applications.
const pageHeader = (title, subtitle) =>
  el("div", { class: "page-header" },
    el("h1", { text: title }),
    subtitle ? el("p", { class: "subtitle", text: subtitle }) : null);

// A cancel is not a failure: the user stopped it.
//
// Rendered as its own state, because the error card says "the console could
// not read this from the local instance" — which would be reporting a fault
// that did not happen, on a screen the user deliberately stopped.
const cancelledState = (title, retry) =>
  el("div", { class: "state" },
    el("h2", { text: title }),
    el("p", { text: "You stopped this request before it finished." }),
    el("button", { class: "secondary", onclick: retry, text: "Try again" }));

const isCancelled = (err) => String(err && err.message) === "cancelled";

const errorState = (title, detail, retry) =>
  el("div", { class: "state error", role: "alert" },
    el("h2", { text: title }),
    el("p", { text: "The console could not read this from the local instance." }),
    el("pre", { text: detail }),
    retry ? el("button", { class: "secondary", onclick: retry, text: "Retry" }) : null);

// --- screens ---------------------------------------------------------

// The cluster utilisation panel.
//
// The numbers are the cluster's own: capacity from the node object, usage from
// the kubelet's summary. Nothing here is estimated, and when the reading
// fails the panel says so rather than drawing an empty bar that reads as
// "idle".
function meterRow(label, used, total, format) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return el("div", { class: "meter" },
    el("div", { class: "meter-head" },
      el("span", { text: label }),
      el("span", { class: "meter-value", text: `${format(used)} / ${format(total)}` })),
    el("div", {
      class: "meter-track", role: "meter", "aria-label": label,
      "aria-valuenow": String(Math.round(pct)), "aria-valuemin": "0", "aria-valuemax": "100",
    },
      el("div", { class: "meter-fill" + (pct >= 90 ? " is-high" : ""),
                  style: `width:${pct.toFixed(1)}%` })),
    el("div", { class: "meter-pct", text: `${pct.toFixed(1)}%` }));
}

const formatCores = (n) => `${n.toFixed(2)} vCPU`;
const formatBytes = (n) => {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
};

// The last reading that actually worked.
//
// Every tick replaced the panel wholesale, so one transient blip — a kubelet
// restart, a momentary timeout — erased a working reading and put an error in
// its place. On a healthy cluster the panel flickered between numbers and a
// failure message. A reading that never moves is worse than none; a reading
// that keeps vanishing is the same problem from the other side.
let LAST_METRICS = null;

// --- charts -----------------------------------------------------------
//
// An inline SVG, drawn from points the server retained. There is no charting
// library here for the same reason there is no framework: the assets ship as
// they are written, and "no external CDN after installation" is true by
// construction rather than by a bundler configuration nobody checks.
//
// The one rule this drawing obeys: a gap is drawn as a gap. A reading that
// failed is not joined to the readings either side, because a straight line
// across a period nobody measured is the chart inventing the thing it exists
// to report.

const CHART_W = 600;
const CHART_H = 120;

function sparkline(points, opts = {}) {
  // A known ceiling — a node's capacity — is the honest scale: it says how
  // much headroom there is, not just how the value moved. Without one the
  // series scales to itself, with a margin so the line is not drawn along the
  // top edge where it reads as saturated.
  const peak = Math.max(...points.map((p) => p.value || 0), 0);
  const max = opts.max || (peak > 0 ? peak * 1.25 : 1);
  const span = points.length > 1 ? points.length - 1 : 1;
  const x = (i) => (i / span) * CHART_W;
  const y = (v) => CHART_H - (Math.min(v, max) / max) * CHART_H;

  // One path per run of consecutive readings. A missing sample ends the run.
  const runs = [];
  let run = [];
  points.forEach((p, i) => {
    if (p.value === null || p.value === undefined) {
      if (run.length) runs.push(run);
      run = [];
      return;
    }
    run.push(`${run.length ? "L" : "M"}${x(i).toFixed(1)},${y(p.value).toFixed(1)}`);
  });
  if (run.length) runs.push(run);

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${CHART_W} ${CHART_H}`);
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("class", "chart");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", opts.label || "chart");

  for (const r of runs) {
    // A single point has no line; a dot is what one reading looks like.
    if (r.length === 1) {
      const dot = document.createElementNS("http://www.w3.org/2000/svg", "circle");
      const [, cx, cy] = r[0].match(/M([\d.]+),([\d.]+)/);
      dot.setAttribute("cx", cx); dot.setAttribute("cy", cy); dot.setAttribute("r", "2");
      dot.setAttribute("class", "chart-point");
      svg.append(dot);
      continue;
    }
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", r.join(" "));
    path.setAttribute("class", "chart-line");
    svg.append(path);
  }
  return svg;
}

// chartCard is a titled chart with its own window and source stated.
//
// Both are on screen rather than assumed: a chart that does not say what it
// covers or where the numbers came from is a picture, not a measurement.
function chartCard(title, points, opts = {}) {
  const readings = points.filter((p) => p.value !== null && p.value !== undefined);
  const body = readings.length < 2
    ? el("p", { class: "unavailable",
        text: readings.length === 1
          ? "Not enough history yet — one reading so far."
          : "Not enough history yet." })
    : sparkline(points, opts);

  return el("div", { class: "chart-card" },
    el("div", { class: "chart-head" },
      el("h3", { text: title }),
      el("span", { class: "chart-now", text: opts.current || "" })),
    body,
    el("p", { class: "chart-foot unavailable", text: opts.foot || "" }));
}

// seriesPoints turns the retained samples into one metric's points.
//
// A sample the server recorded as unavailable becomes a null, which the
// drawing turns into a gap rather than a line.
function seriesPoints(samples, pick) {
  return samples.map((s) => {
    if (s.unavailable || !(s.nodes || []).length) return { at: s.at, value: null };
    return { at: s.at, value: pick(s.nodes[0]) };
  });
}

function renderMetrics(target, m) {
  if (!m.unavailable) LAST_METRICS = { data: m, at: new Date() };

  // Only when there has never been a successful reading is there nothing to
  // show. Anything else keeps the numbers and says how old they are.
  if (m.unavailable && !LAST_METRICS) {
    setChildren(target,
      el("h2", { text: "Cluster utilisation" }),
      el("p", { class: "unavailable", text: m.unavailable }));
    return;
  }

  const shown = m.unavailable ? LAST_METRICS.data : m;
  const at = LAST_METRICS.at;
  target.classList.toggle("is-stale", Boolean(m.unavailable));

  setChildren(target,
    el("h2", { text: "Cluster utilisation" }),
    ...(shown.nodes || []).flatMap((n) => [
      el("div", { class: "meter-node" },
        el("span", { class: "mono", text: n.name }),
        el("span", { class: "status", "data-state": n.ready ? "ok" : "error" },
          el("span", { text: n.ready ? "Ready" : "Not ready" })),
        el("span", { class: "unavailable", text: `${n.pods} pods` })),
      meterRow("CPU", n.cpuUsedCores || 0, n.cpuCapacityCores || 0, formatCores),
      meterRow("Memory", n.memoryUsedBytes || 0, n.memoryTotalBytes || 0, formatBytes),
    ]),
    // Relative, because a formatted clock time looks equally fresh at five
    // seconds and five minutes old — which is exactly the distinction the
    // line exists to make.
    m.unavailable
      ? el("p", { class: "panel-empty metrics-stale", role: "status",
          text: `Last reading ${relativeTime(at)} — refresh failed: ${m.unavailable}` })
      : el("p", { class: "panel-empty", text: `Updated ${relativeTime(at)}` }));
}

async function renderDashboard(view) {
  const cancel = new AbortController();
  setChildren(view, 
    el("h1", { text: "Dashboard" }),
    el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." }),
    loadingState(3, { what: "instance status", onCancel: () => cancel.abort() })
  );

  let status;
  try {
    status = await api("/api/status", { signal: cancel.signal });
  } catch (err) {
    setChildren(view, 
      el("h1", { text: "Dashboard" }),
      isCancelled(err)
        ? cancelledState("Dashboard not loaded", () => renderDashboard(view))
        : errorState("Instance status unavailable", String(err.message),
                     () => renderDashboard(view))
    );
    return;
  }

  const readyState = status.ready ? "ok" : "warn";
  const cards = el("div", { class: "cards" },
    el("div", { class: "card" },
      el("h2", { text: "Instance" }),
      el("dl", {},
        el("dt", { text: "Name" }), el("dd", { text: status.instance || "unknown" }),
        el("dt", { text: "State" }),
        el("dd", {}, el("span", { class: "status", "data-state": readyState },
          el("span", { text: status.state || (status.ready ? "ready" : "starting") }))),
        el("dt", { text: "Mode" }), el("dd", { text: status.mode || "unknown" })
      )),
    el("div", { class: "card" },
      el("h2", { text: "Cluster" }),
      el("dl", {},
        el("dt", { text: "Name" }), el("dd", { text: status.cluster || "unknown" }),
        el("dt", { text: "Kubernetes" }), el("dd", { text: status.kubernetes || "unknown" }),
        el("dt", { text: "Namespace" }), el("dd", { text: status.namespace || "unknown" })
      ))
  );

  const endpoints = Object.entries(status.endpoints || {}).sort();
  cards.append(
    el("div", { class: "card" },
      el("h2", { text: "Endpoints" }),
      endpoints.length
        ? el("dl", {}, endpoints.flatMap(([name, addr]) =>
            [el("dt", { text: name }), el("dd", { text: addr })]))
        : el("p", { class: "unavailable", text: "No endpoints are published." }))
  );

  const services = (status.services || []).map((s) =>
    el("li", {},
      el("span", { class: "status", "data-state": s.enabled ? "ok" : "" },
        el("span", { text: s.title || s.id })),
      s.enabled ? null : el("span", { class: "unavailable", text: ` — ${s.reason || "not enabled"}` }))
  );

  const utilisation = el("div", { class: "card utilisation" },
    el("h2", { text: "Cluster utilisation" }),
    el("p", { class: "unavailable", text: "reading…" }));

  // Component health, read now rather than latched at startup.
  //
  // The card used to render a boolean captured when the instance came up, so
  // a tunnel whose pod had gone away still read as ready — the dashboard
  // answered "did this ever work" while looking like it answered "is this
  // working".
  const components = el("div", { class: "card components" },
    el("h2", { text: "Components" }),
    el("p", { class: "unavailable", text: "reading…" }));

  const drawComponents = (st) => {
    const tunnels = st.tunnels || [];
    const named = Object.entries(st.components || {});
    if (!tunnels.length && !named.length) {
      return setChildren(components,
        el("h2", { text: "Components" }),
        el("p", { class: "unavailable", text: "This instance reports no components." }));
    }
    setChildren(components,
      el("h2", { text: "Components" }),
      el("ul", { class: "component-list" },
        ...named.map(([name, ready]) =>
          el("li", {},
            el("span", { class: "status", "data-state": ready ? "ok" : "warn" },
              el("span", { text: name })))),
        ...tunnels.map((t) =>
          el("li", {},
            el("span", { class: "status", "data-state": t.running ? "ok" : "error" },
              el("span", { text: t.name })),
            el("span", { class: "unavailable mono", text: t.host || "" }),
            // A tunnel that has been re-established is not the same as one
            // that never had to be. Silence about it is how a flapping
            // backend stays invisible.
            t.restarts
              ? el("span", { class: "status", "data-state": "warn" },
                  el("span", { text: `${t.restarts} restart${t.restarts === 1 ? "" : "s"}` }))
              : null))));
  };
  drawComponents(status);

  setChildren(view,
    el("div", { class: "page-header" },
      el("h1", { text: "Dashboard" }),
      el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." })),
    cards,
    el("div", { class: "cards", style: "margin-top:16px" },
      utilisation,
      components,
      el("div", { class: "card" },
        el("h2", { text: "Services" }),
        services.length
          ? el("ul", { style: "list-style:none;margin:0;padding:0" }, services)
          : el("p", { class: "unavailable", text: "No services are enabled." })))
  );

  // Live rather than a snapshot: utilisation that never moves is worse than
  // none, because it reads as a measurement. The timer is cleared by the
  // router when the screen changes, so leaving the dashboard stops the polling
  // instead of leaving it running against a page nobody is looking at.
  const tick = async () => {
    try {
      renderMetrics(utilisation, await api("/api/metrics"));
    } catch (err) {
      renderMetrics(utilisation, { unavailable: String(err.message) });
    }
    // Component health moves on the same tick, for the same reason: a panel
    // that reports a state it captured once is not reporting health.
    try {
      drawComponents(await api("/api/status"));
    } catch { /* the utilisation panel above already says the instance is unreachable */ }
  };
  await tick();
  stopMetrics();
  METRICS_TIMER = setInterval(tick, 5000);

  // Handed to the one visibility listener rather than each dashboard adding
  // its own, which would leave a listener behind on every navigation.
  METRICS_TICK = tick;

  announce("Dashboard loaded");
}

// renderTableInto draws a listing: action bar, filter, sortable table, footer.
//
// Both the list screen and the detail screen call it, so there is one table
// implementation and a row's contents cannot be drawn differently from the row
// it came from. opts.rowControls turns on create, delete, selection and
// per-row actions, which belong to a list and not to the inside of one of its
// rows. opts.refetch, when given, lets the table reload its own data without
// the screen being rebuilt around it.
const PAGE_SIZES = [25, 50, 100];

function renderTableInto(view, header, data, noun, reload, route, opts = {}) {
  const caps = opts.rowControls ? capabilityOf(route.service) : {};
  const selectable = Boolean(opts.rowControls && caps.delete);

  // All of the table's state lives here, so a refresh can put it back.
  let sortColumn = null;
  let sortAscending = true;
  let page = 0;
  let pageSize = PAGE_SIZES[0];
  let selected = new Set();
  // The row the info panel is describing. Separate from `selected`, which is
  // the multi-row selection a bulk delete acts on: "which one am I looking
  // at" and "which ones am I acting on" are different questions.
  let inspected = null;

  // Rows with a request outstanding against them. A row whose delete is in
  // flight looks identical to one that is idle unless something says so, and
  // the user's reading of "nothing happened" is a second click.
  const operating = new Set();

  const nameColumn = () => data.nameColumn || "Name";
  const dataColumns = () => data.columns || [];
  const hasActions = () =>
    data.items.some((i) => (i.actions || []).length) || caps.delete;
  const columns = () => [
    ...(selectable ? ["select"] : []),
    nameColumn(),
    ...dataColumns(),
    // Declared by the provider, or inferred from the rows that happen to be
    // present. Inference alone made the column come and go as the data
    // changed, taking any sort applied to it with it.
    ...(data.alwaysStatus || data.items.some((i) => i.status) ? ["Status"] : []),
    ...(hasActions() ? ["Actions"] : []),
  ];

  const valueOf = (item, column) => {
    if (column === nameColumn()) return item.name || "";
    if (column === "Status") return item.status || "";
    return (item.fields || {})[column] || "";
  };

  // The filter reads every column on screen, not only the name. A filter that
  // silently ignored the columns beside it is worse than none: typing a
  // location and getting nothing reads as "there are none".
  const matches = (item, q) => {
    if (!q) return true;
    if ((item.name || "").toLowerCase().includes(q)) return true;
    if ((item.status || "").toLowerCase().includes(q)) return true;
    return dataColumns().some((c) =>
      String((item.fields || {})[c] || "").toLowerCase().includes(q));
  };

  const filter = el("input", {
    class: "filter", type: "search", placeholder: `Filter ${noun}`,
    "aria-label": `Filter ${noun}`,
  });

  const body = el("tbody");
  const headRow = el("tr");
  const footer = el("div", { class: "table-footer" });
  const bulk = el("button", {
    class: "secondary danger", text: "Delete", disabled: "disabled",
    onclick: () => deleteSelected(),
  });
  const selectionLabel = el("span", { class: "selection-count unavailable", text: "" });

  const visibleRows = () => {
    const q = filter.value.trim().toLowerCase();
    let rows = data.items.filter((i) => matches(i, q));
    if (sortColumn) {
      // Compared numerically when both sides are numbers, so "10" does not
      // sort before "9", and case-insensitively otherwise.
      rows = [...rows].sort((a, b) => {
        const x = valueOf(a, sortColumn), y = valueOf(b, sortColumn);
        const nx = Number(x), ny = Number(y);
        const cmp = (x !== "" && y !== "" && !Number.isNaN(nx) && !Number.isNaN(ny))
          ? nx - ny
          : x.toLowerCase().localeCompare(y.toLowerCase());
        return sortAscending ? cmp : -cmp;
      });
    }
    return rows;
  };

  const deleteSelected = () => {
    const names = [...selected];
    if (!names.length) return;
    confirmDestructive({
      title: `Delete ${names.length} ${names.length === 1 ? noun.replace(/s$/, "") : noun}?`,
      detail: names.join(", "),
      confirmWord: names.length === 1 ? names[0] : String(names.length),
      onConfirm: async () => {
        for (const name of names) operating.add(name);
        draw();
        try {
          for (const name of names) {
            await send(`/api/resources/${route.service}?project=` +
              `${encodeURIComponent(currentProject())}&name=${encodeURIComponent(name)}`, "DELETE");
            operating.delete(name);
            draw();
          }
        } finally {
          // A failure stops the loop, so the rows it never reached must not
          // be left looking busy forever.
          for (const name of names) operating.delete(name);
          draw();
        }
        selected = new Set();
        refresh();
      },
    });
  };

  // A handle the action functions use to say when a row's request starts and
  // stops. Redrawing is cheap and keeps one code path drawing rows.
  const rowBusy = (name) => ({
    start() { operating.add(name); draw(); },
    end() { operating.delete(name); draw(); },
  });

  const rowActionsCell = (item) => {
    const busy = operating.has(item.name);
    const actions = [
      ...(item.actions || []).map((a) => ({
        label: a.label, destructive: a.destructive,
        run: () => runAction(route, item.name, a, refresh, rowBusy(item.name)),
      })),
      ...(caps.delete ? [{
        label: "Delete", destructive: true,
        run: () => deleteResource(route, item.name, refresh, rowBusy(item.name)),
      }] : []),
    ];
    if (!actions.length) return el("td", {});
    // Suppressed rather than merely ignored while the row is busy: a live
    // menu over an in-flight request is an invitation to issue a second one,
    // and Pause has no confirmation step to catch it.
    if (busy) {
      return el("td", { class: "row-actions" },
        el("button", { class: "icon-button overflow-trigger", disabled: true,
          "aria-label": `Actions for ${item.name} are unavailable while it is being changed`,
          html: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg>' }));
    }
    // Behind an overflow menu: a row of buttons competes with the data for
    // attention, and the console this mirrors puts them behind one control.
    return el("td", { class: "row-actions" }, overflowMenu(actions, item.name));
  };

  const draw = () => {
    const rows = visibleRows();
    const total = rows.length;
    const pages = Math.max(1, Math.ceil(total / pageSize));
    if (page >= pages) page = pages - 1;
    const start = page * pageSize;
    const shown = rows.slice(start, start + pageSize);

    setChildren(body, ...shown.map((item) => {
      const cells = [];
      if (selectable) {
        cells.push(el("td", { class: "select-cell" },
          el("input", {
            type: "checkbox", "aria-label": `Select ${item.name}`,
            checked: selected.has(item.name) ? "checked" : null,
            onchange: (e) => {
              if (e.target.checked) selected.add(item.name);
              else selected.delete(item.name);
              drawSelection();
            },
          })));
      }
      cells.push(el("td", {}, item.link
        ? el("a", { href: item.link, text: item.name })
        : caps.detail
          ? el("a", { href: detailHref(route, item.name), text: item.name })
          : document.createTextNode(item.name)));
      for (const c of dataColumns()) {
        cells.push(el("td", { text: (item.fields || {})[c] || "—" }));
      }
      const busy = operating.has(item.name);
      if (columns().includes("Status")) {
        // The status the row had is not the status it has: while a request is
        // outstanding the cell says "working", not the value it is about to
        // stop being.
        cells.push(busy
          ? el("td", {}, el("span", { class: "status is-working" },
              spinner(`${item.name} is being changed`)))
          : el("td", {}, el("span", { class: "status", "data-state": stateOf(item.status) },
              el("span", { text: item.status || "—" }))));
      }
      if (hasActions()) cells.push(rowActionsCell(item));
      const row = el("tr", {
        class: [selected.has(item.name) ? "is-selected" : "",
                busy ? "is-operating" : "",
                inspected === item.name ? "is-inspected" : ""]
          .filter(Boolean).join(" ") || null,
      }, ...cells);
      if (opts.rowControls) {
        // A click anywhere that is not itself a control inspects the row. A
        // link or a checkbox keeps doing its own job.
        row.addEventListener("click", (e) => {
          if (e.target.closest("a, button, input, select, label")) return;
          inspected = item.name;
          draw();
          drawInfoPanel(item, dataColumns(), route, refresh);
        });
      }
      return row;
    }));

    if (!shown.length) {
      // An empty filter result is its own state: "no matches for X" is a
      // different fact from "you have none", and only one of them is a reason
      // to go and create something.
      //
      // Which one this is depends on the filter, not on the row count. A
      // table emptied by deleting its last row was offering to clear a filter
      // nobody had typed, and calling nothing a non-match.
      const query = filter.value.trim();
      const state = query
        ? el("div", { class: "state state-inline" },
            el("h2", { text: `No matching ${noun}` }),
            el("p", { text: `Nothing matches “${query}”.` }),
            el("button", {
              class: "secondary", text: "Clear filter",
              onclick: () => { filter.value = ""; page = 0; draw(); },
            }))
        : el("div", { class: "state state-inline" },
            el("h2", { text: `No ${noun} yet` }),
            el("p", { text: "Create one here, or with an SDK, the CLI or gcloud — " +
                            "it will appear either way." }),
            caps.create
              ? el("button", { class: "primary", text: caps.create.label,
                  onclick: () => startCreate(route, caps.create, refresh) })
              : null);
      setChildren(body, el("tr", {},
        el("td", { colspan: String(columns().length) }, state)));
    }

    drawHead();
    drawFooter(total, start, shown.length);
    drawSelection();
  };

  const drawFooter = (total, start, count) => {
    drawFreshness();
    const pages = Math.max(1, Math.ceil(total / pageSize));
    const from = count ? start + 1 : 0;
    const to = start + count;
    setChildren(footer,
      el("label", { class: "page-size" },
        el("span", { text: "Rows per page" }),
        el("select", {
          "aria-label": "Rows per page",
          onchange: (e) => { pageSize = Number(e.target.value); page = 0; draw(); },
        }, ...PAGE_SIZES.map((n) =>
          el("option", { value: String(n), text: String(n), selected: n === pageSize ? "selected" : null })))),
      // The range follows the filter, so the count on screen always describes
      // the rows on screen.
      el("span", { class: "page-range", text: `${from}–${to} of ${total}` }),
      el("button", {
        class: "icon-button", "aria-label": "Previous page",
        disabled: page === 0 ? "disabled" : null,
        onclick: () => { page--; draw(); },
        html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15 6l-6 6 6 6"/></svg>',
      }),
      el("button", {
        class: "icon-button", "aria-label": "Next page",
        disabled: page >= pages - 1 ? "disabled" : null,
        onclick: () => { page++; draw(); },
        html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>',
      }),
      // The pager above moves through rows already fetched. This fetches more.
      //
      // The backend bounds every listing, so without this the rows past that
      // bound were unreachable from the console at all — the pager moved
      // through a fixed set and the note said "there may be more".
      loadMore());
  };

  // loadMore fetches the rows after the ones on screen and appends them.
  const loadMore = () => {
    if (!data.more || !data.cursor || !opts.pagePath) return null;
    const button = el("button", { class: "secondary", text: "Load more" });
    button.addEventListener("click", async () => {
      setBusy(button, true);
      try {
        const url = new URL(`/api/page/${route.service}`, location.origin);
        url.searchParams.set("project", currentProject());
        url.searchParams.set("cursor", data.cursor);
        for (const segment of opts.pagePath) url.searchParams.append("name", segment);
        const next = await api(url.pathname + url.search);
        // Appended rather than replacing: the rows on screen stay on screen, and
        // a sort or filter the user set up keeps applying to all of them.
        data.items = [...data.items, ...(next.items || [])];
        data.total = data.items.length;
        data.cursor = next.cursor || "";
        data.more = Boolean(next.more);
        announce(`${(next.items || []).length} more rows loaded`);
        draw();
      } catch (err) {
        notify(`Could not load more: ${err.message}`, "error");
        setBusy(button, false);
      }
    });
    return button;
  };

  const drawSelection = () => {
    if (!selectable) return;
    const n = selected.size;
    bulk.disabled = n === 0;
    selectionLabel.textContent = n ? `${n} selected` : "";
    const boxes = [...body.querySelectorAll('input[type="checkbox"]')];
    const head = headRow.querySelector('input[type="checkbox"]');
    if (head) {
      const onPage = visibleRows().slice(page * pageSize, page * pageSize + pageSize);
      const chosen = onPage.filter((i) => selected.has(i.name)).length;
      head.checked = chosen > 0 && chosen === onPage.length;
      head.indeterminate = chosen > 0 && chosen < onPage.length;
    }
    for (const box of boxes) {
      const row = box.closest("tr");
      if (row) row.classList.toggle("is-selected", box.checked);
    }
  };

  const drawHead = () => {
    setChildren(headRow, ...columns().map((c) => {
      if (c === "select") {
        return el("th", { class: "select-cell", scope: "col" },
          el("input", {
            type: "checkbox", "aria-label": `Select all ${noun} on this page`,
            onchange: (e) => {
              const onPage = visibleRows().slice(page * pageSize, page * pageSize + pageSize);
              for (const item of onPage) {
                if (e.target.checked) selected.add(item.name);
                else selected.delete(item.name);
              }
              draw();
            },
          }));
      }
      // Actions is a column of controls, not of values, so it does not sort:
      // offering it would be a control that does nothing.
      if (c === "Actions") return el("th", { scope: "col", text: c });
      const active = sortColumn === c;
      const arrow = active ? (sortAscending ? "↑" : "↓") : "";
      return el("th", {
        scope: "col",
        "aria-sort": active ? (sortAscending ? "ascending" : "descending") : "none",
      },
        el("button", {
          class: "sort-button" + (active ? " is-active" : ""),
          onclick: () => {
            if (sortColumn === c) sortAscending = !sortAscending;
            else { sortColumn = c; sortAscending = true; }
            draw();
            announce(`Sorted by ${c}, ${sortAscending ? "ascending" : "descending"}`);
          },
        },
          el("span", { text: c }),
          el("span", { class: "sort-arrow", "aria-hidden": "true", text: arrow })));
    }));
  };

  // Refreshing reloads the rows without rebuilding the screen, so sort, filter,
  // page and scroll position survive. Re-rendering the whole screen threw all
  // of that away and flashed a skeleton over data that was already correct.
  // When the rows were last read, and whether the last attempt worked. A
  // list that goes quietly stale is a list that lies by omission.
  let readAt = new Date();
  let staleBecause = "";
  const freshness = el("p", { class: "table-freshness unavailable", role: "status" });

  const drawFreshness = () => {
    setChildren(freshness,
      el("span", { text: staleBecause
        ? `Last read ${relativeTime(readAt)} — refresh failed: ${staleBecause}`
        : `Updated ${relativeTime(readAt)}` }));
    freshness.classList.toggle("is-stale", Boolean(staleBecause));
  };

  // quiet is the automatic poll: it must not raise a snackbar every interval
  // on an instance whose backend is down, and must not blank a table that
  // still holds the last good rows.
  const refresh = async (quiet = false) => {
    if (!opts.refetch) return reload();
    try {
      const fresh = await opts.refetch();
      if (fresh && Array.isArray(fresh.items)) {
        data = fresh;
        // A selection may name rows that no longer exist.
        const names = new Set(data.items.map((i) => i.name));
        selected = new Set([...selected].filter((n) => names.has(n)));
        readAt = new Date();
        staleBecause = "";
        draw();
        announce(`${data.items.length} ${noun}, updated just now`);
        return;
      }
    } catch (err) {
      staleBecause = err.message;
      drawFreshness();
      if (quiet) return;
      notify(`Could not refresh: ${err.message}`, "error");
    }
    if (quiet) return;
    reload();
  };

  // The manual control says it is working for as long as it is, and clears
  // in a finally so a failure does not strand the spinner.
  const refreshButton = el("button", { class: "secondary", text: "Refresh" });
  refreshButton.addEventListener("click", async () => {
    setBusy(refreshButton, true);
    try { await refresh(); } finally { setBusy(refreshButton, false); }
  });

  // The automatic poll. Registered so the router clears it, and suspended
  // for a hidden tab through the one visibility listener rather than a
  // second mechanism.
  if (opts.refetch) {
    registerListPoll(() => refresh(true));
  }

  // The panel's open state is the viewer's, not the screen's: it stays open
  // across navigations the way a docked region should.
  const infoToggle = () => {
    const open = infoPanelOpen();
    const button = el("button", {
      class: "secondary", "aria-expanded": open ? "true" : "false",
      "aria-controls": "info-panel",
      text: open ? "Hide info panel" : "Show info panel",
      onclick: () => {
        if (infoPanelOpen()) return closeInfoPanel();
        writeStored(INFO_OPEN_KEY, true);
        const panel = infoPanelHost();
        panel.hidden = false;
        document.documentElement.setAttribute("data-info", "open");
        button.setAttribute("aria-expanded", "true");
        button.textContent = "Hide info panel";
        const item = data.items.find((i) => i.name === inspected);
        drawInfoPanel(item, dataColumns(), route, refresh);
        panel.focus();
      },
    });
    INFO_TOGGLE = button;
    return button;
  };

  // Restored on render, so a screen change does not close it.
  if (opts.rowControls && infoPanelOpen()) {
    const panel = infoPanelHost();
    if (panel) {
      panel.hidden = false;
      document.documentElement.setAttribute("data-info", "open");
      drawInfoPanel(null, dataColumns(), route, refresh);
    }
  } else if (!opts.rowControls) {
    // A screen with no rows to inspect has nothing to put in it.
    const panel = infoPanelHost();
    if (panel) panel.hidden = true;
    document.documentElement.removeAttribute("data-info");
  }

  filter.addEventListener("input", () => { page = 0; draw(); });
  drawFreshness();
  draw();

  setChildren(view, ...header,
    data.note ? el("p", { class: "unavailable", text: data.note }) : null,
    // The page's actions and the table's filter are different things, so they
    // are different bars: one acts on the product, the other narrows the view.
    opts.rowControls
      ? el("div", { class: "action-bar" },
          caps.create
            ? el("button", { class: "primary", text: caps.create.label,
                onclick: () => startCreate(route, caps.create, refresh) })
            : null,
          selectable ? bulk : null,
          selectionLabel,
          refreshButton,
          infoToggle())
      : el("div", { class: "action-bar" }, refreshButton),
    el("div", { class: "filter-bar" }, filter, freshness),
    el("div", { class: "table-wrap" },
      el("table", {}, el("thead", {}, headRow), body)),
    footer);
}

// detailHref is the address of one row's contents.
// detailHref is a resource's own address, one path segment per level.
//
// Readable as text, which docs/console-parity.md section 5 requires, and
// deep-linkable at any depth: /cloudsql/cloudburrow/widgets is a table.
function detailHref(route, path) {
  const segments = Array.isArray(path) ? path : [path];
  const url = new URL(
    route.path + "/" + segments.map(encodeURIComponent).join("/"),
    location.origin);
  const project = new URLSearchParams(location.search).get("project");
  if (project) url.searchParams.set("project", project);
  return url.pathname + url.search;
}

// renderDetail shows what is inside one row.
//
// It draws the provider's listing with the same renderer as the list screen:
// one table implementation, so the two cannot drift apart, and sorting and
// filtering work here for free.
async function renderDetail(view, route, resourcePath) {
  const segments = Array.isArray(resourcePath) ? resourcePath : [resourcePath];
  const name = segments[segments.length - 1];
  const project = new URLSearchParams(location.search).get("project") || "";
  const back = new URL(route.path, location.origin);
  if (project) back.searchParams.set("project", project);

  // One crumb per level, built from the path rather than from two fixed
  // nodes. A database, a table and a column are three levels, and every one
  // above the last is a working link back to it.
  const trail = [
    el("a", { href: back.pathname + back.search, text: route.title }),
  ];
  segments.forEach((segment, i) => {
    trail.push(el("span", { "aria-hidden": "true", text: "/" }));
    trail.push(i === segments.length - 1
      ? el("span", { text: segment })
      : el("a", { href: detailHref(route, segments.slice(0, i + 1)), text: segment }));
  });

  const crumb = el("div", { class: "page-header" },
    // A breadcrumb, because a screen you can only leave with the browser
    // button is a screen you are stuck in.
    el("nav", { class: "breadcrumb", "aria-label": "Breadcrumb" }, ...trail),
    el("h1", { text: name }),
    el("p", { class: "subtitle", text: project ? `Project ${project}` : "All projects" }));
  const header = [crumb];

  const path = `/api/detail/${route.service}?project=${encodeURIComponent(project)}` +
               segments.map((sg) => `&name=${encodeURIComponent(sg)}`).join("");
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(5, { what: name, onCancel: () => cancel.abort() }));

  let data;
  try {
    data = await api(path, { signal: cancel.signal });
  } catch (err) {
    return setChildren(view, ...header,
      isCancelled(err)
        ? cancelledState(`${name} not loaded`, () => renderDetail(view, route, segments))
        : errorState(`${name} unavailable`, String(err.message),
                     () => renderDetail(view, route, segments)));
  }
  // The prompt is checked first, for the same reason it is on the list
  // screen: needing a project is a precondition, not a failure, and it must
  // never be rendered as one.
  if (data.prompt) {
    return setChildren(view, ...header, emptyState("Choose a project", data.prompt));
  }
  if (data.unavailable) {
    return setChildren(view, ...header,
      errorState(`${name} unavailable`, data.unavailable, () => renderDetail(view, route, segments)));
  }

  const sections = (data.sections || []).slice();
  // The tab is offered only where the backend can actually answer, which is
  // the same rule the create button follows: a control appears when the
  // service behind it can perform the operation, and is absent otherwise.
  const queryable = capabilityOf(route.service).query;
  if (queryable) {
    // The provider may narrow the form to the resource being looked at — a
    // Bigtable table's column families are not a Firestore collection's fields —
    // so the resource's own query spec wins over the service-wide one.
    sections.push({
      id: "query", label: "Query", kind: "query",
      query: data.query || queryable,
    });
  }
  if (!sections.length) {
    return setChildren(view, ...header,
      emptyState(`Nothing to show for ${name}`,
        "This resource reports no sections, which is a gap in the provider rather than a fault."));
  }

  // What this is, before what is inside it.
  //
  // The properties are the ones the list row already showed; clicking through
  // used to drop them, so the screen named after a resource said nothing
  // about it. A provider that holds none renders no card rather than an empty
  // one.
  const summary = (data.summary || []).length
    ? el("div", { class: "card properties" },
        el("h2", { text: "Details" }),
        el("dl", {}, ...(data.summary).flatMap((prop) => [
          el("dt", { text: prop.label }),
          el("dd", { text: prop.value }),
        ])))
    : null;

  const wanted = new URLSearchParams(location.search).get("tab");
  let current = Math.max(0, sections.findIndex((sec) => sec.id === wanted));

  const panel = el("div", { class: "tab-panel", id: "detail-panel", role: "tabpanel" });

  const drawPanel = () => {
    const section = sections[current];
    const reload = () => renderDetail(view, route, segments);

    // A section that cannot be read says so inside its own panel, whatever
    // kind it is. Rendering it as an empty table would claim the resource
    // holds nothing.
    const failure = section.unavailable || (section.listing || {}).unavailable;
    if (failure) {
      return setChildren(panel,
        errorState(`${section.label} unavailable`, failure, reload));
    }

    const note = section.note
      ? el("p", { class: "unavailable", text: section.note })
      : null;

    // The provider says what the section holds. Inferring it from whichever
    // field happened to be populated would put the decision in the client,
    // and an empty one would be indistinguishable from a table with no rows.
    switch (section.kind) {
      case "properties":
        return drawPropertiesSection(panel, section, note);
      case "text":
        return drawTextSection(panel, section, note);
      case "chart":
        return drawChartSection(panel, section, note);
      case "query":
        return setChildren(panel,
          queryPane(route, segments, section.query, () => {}));
      case undefined:
      case "":
      case "listing":
        return drawListingSection(panel, section, note, reload);
      default:
        // Said out loud rather than drawn as an empty table: a kind this
        // console does not know is a version skew, and guessing hides it.
        return setChildren(panel, emptyState(
          `This console cannot draw a ${section.kind} section`,
          "The instance is serving a section kind this console does not know how to render."));
    }
  };

  const drawListingSection = (into, section, note, reload) => {
    let list = section.listing || {};
    const noun = list.noun || section.label.toLowerCase();
    if (!(list.items || []).length) {
      return setChildren(into, note,
        emptyState(`No ${noun}`, `${name} holds no ${noun} yet.`));
    }
    setChildren(into, note);
    // A row that has a level below it becomes a link into that level. The
    // provider declares it; the client neither guesses nor offers a link
    // that would 501.
    if (list.rowsOpenable || (list.items || []).some((i) => i.opens)) {
      list = {
        ...list,
        items: list.items.map((item) => {
          // A row's own path wins: a listing can mix rows that open with
          // rows that do not, which is what a bucket's folders and objects
          // are.
          if (item.opens) return { ...item, link: detailHref(route, item.opens) };
          if (!list.rowsOpenable) return item;
          return { ...item, link: item.link || detailHref(route, [...segments, item.name]) };
        }),
      };
    }
    renderTableInto(into, note ? [note] : [], list, noun, reload, route, {
      pagePath: segments,
      refetch: async () => {
        const fresh = await api(path);
        const same = (fresh.sections || []).find((sec) => sec.id === section.id);
        return same ? same.listing : null;
      },
    });
  };

  // Properties render as the same definition list the summary card uses, so a
  // resource's configuration and its identity are one visual language rather
  // than two.
  const drawPropertiesSection = (into, section, note) => {
    const groups = (section.groups || []).filter((g) => (g.properties || []).length);
    if (!groups.length) {
      return setChildren(into, note,
        emptyState(`No ${section.label.toLowerCase()}`,
                   `${name} reports none.`));
    }
    setChildren(into, note, ...groups.map((group) =>
      el("div", { class: "card properties" },
        group.heading ? el("h2", { text: group.heading }) : null,
        el("dl", {}, ...group.properties.flatMap((prop) => [
          el("dt", { text: prop.label }),
          el("dd", { text: prop.value }),
        ])))));
  };

  // Preformatted: newlines are the content, and a YAML document that lost
  // them is not a YAML document.
  const drawTextSection = (into, section, note) => {
    if (!section.text) {
      return setChildren(into, note,
        emptyState(`No ${section.label.toLowerCase()}`, `${name} reports none.`));
    }
    setChildren(into, note,
      el("div", { class: "card text-section" },
        el("div", { class: "text-actions" },
          copyButton(section.text, `${section.label} for ${name}`)),
        el("pre", { class: "mono", text: section.text })));
  };

  const drawChartSection = (into, section, note) => {
    const series = (section.series || []).filter((sv) => (sv.points || []).length);
    if (!series.length) {
      return setChildren(into, note,
        emptyState("No readings yet",
          "This instance has taken no measurements for this resource yet."));
    }
    setChildren(into, note, el("div", { class: "charts" }, ...series.map((sv) =>
      chartCard(sv.label, sv.points, {
        label: `${sv.label} for ${name}`,
        max: sv.max || undefined,
        foot: sv.unit ? `Measured in ${sv.unit}` : "",
      }))));
  };

  // One section is not a tab strip. A strip of one is a control that does
  // nothing, and the real console does not draw one either.
  let strip = null;
  if (sections.length > 1) {
    const tabs = sections.map((section, i) =>
      el("button", {
        class: "tab" + (i === current ? " is-selected" : ""),
        role: "tab", id: `tab-${section.id}`,
        "aria-selected": i === current ? "true" : "false",
        "aria-controls": "detail-panel",
        tabindex: i === current ? "0" : "-1",
        text: section.label,
        onclick: () => select(i),
      }));

    const select = (i) => {
      current = i;
      tabs.forEach((tab, j) => {
        tab.classList.toggle("is-selected", j === i);
        tab.setAttribute("aria-selected", j === i ? "true" : "false");
        tab.setAttribute("tabindex", j === i ? "0" : "-1");
      });
      // The tab is part of the address, so a colleague can be sent the tab
      // rather than the resource.
      const url = new URL(location.href);
      url.searchParams.set("tab", sections[i].id);
      history.replaceState({}, "", url);
      panel.setAttribute("aria-labelledby", `tab-${sections[i].id}`);
      drawPanel();
      tabs[i].focus();
      announce(`${sections[i].label} selected`);
    };

    strip = el("div", { class: "tab-strip", role: "tablist", "aria-label": `${name} sections` },
      ...tabs);
    // Arrow keys move between tabs, which is what makes a tablist a tablist
    // rather than a row of buttons.
    strip.addEventListener("keydown", (e) => {
      const step = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1
        : e.key === "Home" ? -current : e.key === "End" ? sections.length - 1 - current : 0;
      if (!step && e.key !== "Home" && e.key !== "End") return;
      e.preventDefault();
      select((current + step + sections.length) % sections.length);
    });
    crumb.append(strip);
  }

  // What can be done to this resource, on the resource's own page.
  //
  // The provider decides; the server checks the same list before performing
  // one, so a button that is absent here is also refused there. An edit form
  // is offered only when the resource carries one — what may be changed about
  // a queue is not what may be changed about a subscription, so the form
  // belongs to the resource and not to the service.
  const reloadPage = () => renderDetail(view, route, segments);
  const pageActions = [
    ...(data.actions || []).map((a) =>
      el("button", {
        class: "secondary" + (a.destructive ? " danger" : ""),
        text: a.label,
        onclick: () => runAction(route, segments, a, reloadPage),
      })),
  ];
  if (data.reveal) {
    pageActions.push(el("button", {
      class: "secondary", text: data.reveal,
      onclick: () => revealValue(route, segments, data.reveal),
    }));
  }
  if (data.edit) {
    pageActions.unshift(el("button", {
      class: "primary", text: data.edit.label || "Edit",
      onclick: () => openEditForm(route, segments, data.edit, reloadPage),
    }));
  }
  if (pageActions.length) {
    crumb.append(el("div", { class: "page-actions" }, ...pageActions));
  }

  panel.setAttribute("aria-labelledby", `tab-${sections[current].id}`);
  setChildren(view, ...header, summary, panel);
  drawPanel();
  announce(`${name} opened`);
}

// openActionForm collects an action's inputs and then performs it.
function openActionForm(route, segments, action, onDone) {
  const name = segments[segments.length - 1];
  const fields = buildCreateForm({ label: action.label, fields: action.fields });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;

  const { dialog, close } = openModal({
    labelledBy: "action-title",
    canClose: () => !submitting,
  });

  const primary = el("button", {
    type: "submit",
    class: action.destructive ? "primary danger" : "primary",
    text: action.label,
  });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`${action.label} ${name}`);
    try {
      const res = await send(
        `/api/actions/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Action: action.id, Values: fields.values() });
      op.succeeded("", res.operation);
      submitting = false;
      close();
      notify(`${action.label} applied to ${name}`);
      onDone();
    } catch (err) {
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "action-title", text: `${action.label} for ${name}` }),
    error,
    ...fields.nodes,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

// revealValue asks for a resource's secret value and shows it once.
//
// The value is fetched when the button is pressed, never with the page, and it
// is not written into the URL, the history or any listing. It is dropped when
// the dialog closes, because a secret left on screen behind whatever the
// operator does next is a secret on a shared screen.
async function revealValue(route, segments, label) {
  const name = segments[segments.length - 1];
  let res;
  try {
    res = await send(
      `/api/reveal/${route.service}?project=${encodeURIComponent(currentProject())}`,
      "POST", { Path: segments });
  } catch (err) {
    return notify(`Could not read ${name}: ${err.message}`, "error");
  }

  const { dialog, close } = openModal({ labelledBy: "reveal-title" });
  const value = el("pre", { class: "mono reveal-value", text: res.value });
  dialog.append(el("div", { class: "modal-body" },
    el("h2", { id: "reveal-title", text: res.label || label }),
    el("p", { class: "form-help",
      text: "This access was recorded in Activity. Close this dialog when you " +
            "are done; the value is not kept." }),
    value,
    el("div", { class: "modal-actions" },
      copyButton(res.value, `the value of ${name}`),
      el("button", { type: "button", class: "primary", text: "Done",
                     onclick: () => close() }))));
  dialog.querySelector(".modal-actions button:last-child").focus();
}

// openEditForm changes a resource in place.
//
// It reuses the create form wholesale — the same validation, the same grouping,
// the same discard prompt — because an edit form that looked or behaved
// differently from a create form would be a second form implementation to keep
// in step with the first.
function openEditForm(route, segments, spec, onDone) {
  const name = segments[segments.length - 1];
  const fields = buildCreateForm(spec);
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  let submitting = false;

  // Dismissing mid-save would leave the change completing against a form
  // that no longer exists, and the operator with no idea whether it applied.
  const { dialog, close } = openModal({
    labelledBy: "edit-title",
    canClose: () => !submitting,
  });

  const primary = el("button", { type: "submit", class: "primary", text: spec.label || "Save" });
  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (submitting || !fields.validate()) return;
    submitting = true;
    primary.disabled = true;
    const op = recordOperation(`Update ${name}`);
    try {
      const res = await send(
        `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "PATCH", { Path: segments, Values: fields.values() });
      op.succeeded("", res.operation);
      submitting = false;
      close();
      notify(`Updated ${name}`);
      onDone();
    } catch (err) {
      // The API's own message, on the form rather than in a snackbar: the
      // reason a change was refused belongs where the change was made.
      op.failed(err.message, err.operation);
      error.textContent = err.message;
      error.hidden = false;
      submitting = false;
      primary.disabled = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "edit-title", text: `${spec.label || "Edit"} ${name}` }),
    spec.note ? el("p", { class: "form-help", text: spec.note }) : null,
    error,
    ...fields.nodes,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

async function renderList(view, route) {
  const project = new URLSearchParams(location.search).get("project") || "";

  const header = [
    el("div", { class: "page-header" },
      el("h1", { text: route.title }),
      el("p", { class: "subtitle",
                text: project ? `Project ${project}` : "All projects" })),
  ];
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(5, { what: route.title.toLowerCase(), onCancel: () => cancel.abort() }));

  let data;
  try {
    data = await api(`/api/resources/${route.service}?project=${encodeURIComponent(project)}`,
      { signal: cancel.signal });
  } catch (err) {
    setChildren(view, ...header,
      isCancelled(err)
        ? cancelledState(`${route.title} not loaded`, () => renderList(view, route))
        : errorState(`${route.title} unavailable`, String(err.message),
                     () => renderList(view, route)));
    return;
  }

  // The plural word for these rows, used by the filter, the empty state and
  // the announcements. Declared here because every branch below needs it.
  const noun = data.noun || route.title.toLowerCase();

  // A screen that needs something from the user is not a broken screen. This
  // is rendered as a prompt rather than as an error, because a red failure
  // box for "choose a project" taught the user a working instance was broken.
  if (data.prompt) {
    setChildren(view, ...header, emptyState(`Choose a project`, data.prompt));
    announce(data.prompt);
    return;
  }

  // An unreachable backend is an error state, never an empty table: an empty
  // table says "you have none", which sends a developer to debug their code.
  if (data.unavailable) {
    setChildren(view, ...header,
      errorState(`${route.title} unavailable`, data.unavailable, () => renderList(view, route)));
    announce(`${route.title} unavailable`);
    return;
  }

  if (!data.items.length) {
    const caps = capabilityOf(route.service);
    const empty = emptyState(`No ${noun} yet`,
      "Create one here, or with an SDK, the CLI or gcloud — it will appear either way.");
    if (caps.create) {
      empty.append(el("button", { class: "primary", text: caps.create.label,
        onclick: () => startCreate(route, caps.create, () => renderList(view, route)) }));
    }
    setChildren(view, ...header, empty);
    announce(`No ${noun}`);
    return;
  }

  renderTableInto(view, header, data, noun, () => renderList(view, route), route,
                  {
                    rowControls: true,
                    // An empty path is the list screen itself, which is what the
                    // page route reads it as.
                    pagePath: [],
                    // Lets the table reload its own rows without the screen
                    // being rebuilt around it, so sort, filter, page and
                    // scroll position survive a refresh.
                    refetch: () => api(
                      `/api/resources/${route.service}?project=${encodeURIComponent(project)}`),
                  });
  announce(`${data.items.length} ${noun} loaded`);
}

// --- modals -----------------------------------------------------------
//
// One shell for every dialog. Focus is captured on open and returned to the
// control that opened it, Tab cycles inside the dialog instead of walking off
// behind it, and the page underneath is inert. Written once, because "a
// dialog that traps focus" and "a dialog that does not" are not two designs;
// they are one design and one bug.
//
// canClose is asked before every dismissal and is told which one it is: a
// stray backdrop click and a deliberate Escape are different intentions, and
// a form with typed input in it should survive one of them.

function openModal({ labelledBy, canClose = () => true }) {
  const dialog = el("div", { class: "modal", role: "dialog", "aria-modal": "true",
                             "aria-labelledby": labelledBy });
  const opener = document.activeElement;

  // Only the nodes this modal marked are unmarked on close. inert set on an
  // ancestor cannot be cancelled on a descendant, so a second dialog that
  // blindly cleared the flag would wake the page up underneath the first.
  //
  // The live region is exempt. inert takes its subtree out of the
  // accessibility tree, which would silently swallow every announcement made
  // while a dialog is open — including the one that says the create
  // succeeded, which is made from inside the dialog.
  const inerted = [...document.body.children]
    .filter((n) => !n.inert && n.id !== "live" && n.id !== "snackbars");

  const focusable = () => [...dialog.querySelectorAll(
    "a[href], button, input, select, textarea, [tabindex]")]
    .filter((n) => !n.disabled && n.tabIndex !== -1 && !n.closest("[hidden]"));

  const close = (reason = "explicit") => {
    if (!canClose(reason)) return false;
    // Removed after the exit, but made inert and untouchable straight away:
    // a dialog that is on its way out must not still be clickable, and the
    // page behind it must come back immediately.
    dialog.classList.remove("is-open");
    dialog.inert = true;
    setTimeout(() => dialog.remove(), OVERLAY_EXIT_MS);
    for (const n of inerted) n.inert = false;
    // Back to the button that opened it, not to the top of the page: a
    // keyboard user who cancels a create should be where they started.
    if (opener && opener.isConnected && opener.focus) opener.focus();
    else document.getElementById("main").focus();
    // Announced rather than returned, because the three dismissal paths —
    // Escape, the backdrop and a button — all land here and a caller that
    // needs to know it is gone should not have to hook each of them.
    dialog.dispatchEvent(new CustomEvent("cb-closed"));
    return true;
  };

  dialog.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.preventDefault(); close("escape"); return; }
    if (e.key !== "Tab") return;
    const items = focusable();
    if (items.length < 2) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  dialog.addEventListener("click", (e) => { if (e.target === dialog) close("backdrop"); });

  for (const n of inerted) n.inert = true;
  document.body.append(dialog);
  requestAnimationFrame(() => dialog.classList.add("is-open"));
  return { dialog, close };
}

// copyButton copies a value and says it did.
//
// The real console offers this on every identifier and endpoint. Here it
// matters most on a YAML pane, where the alternative is selecting a hundred
// lines by hand.
function copyButton(value, what) {
  const button = el("button", { class: "secondary", text: "Copy" });
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(value);
      notify(`Copied ${what}`);
    } catch (err) {
      // The clipboard API needs a secure context and a user gesture, and
      // refuses in some browsers regardless. Saying so beats a button that
      // silently does nothing.
      notify(`Could not copy: ${err.message}`, "error");
    }
  });
  return button;
}

// spinner is the one indeterminate indicator in the console.
//
// Silent unless it is given a label: a decorative spinner beside text that
// already says what is happening should not be read out twice.
function spinner(label) {
  return el("span", {
    class: "spinner",
    role: label ? "status" : null,
    "aria-label": label || null,
    "aria-hidden": label ? null : "true",
  });
}

// setBusy puts a button in a busy state without taking its label away.
//
// A button whose text is swapped for "Working…" stops saying what it will do,
// and by the time it comes back the user has forgotten what they clicked.
function setBusy(button, busy) {
  if (!button) return;
  button.disabled = busy;
  button.classList.toggle("is-busy", busy);
  if (busy) {
    button.setAttribute("aria-busy", "true");
    if (!button.querySelector(".spinner")) button.prepend(spinner());
  } else {
    button.removeAttribute("aria-busy");
    const existing = button.querySelector(".spinner");
    if (existing) existing.remove();
  }
}

// --- the info panel ---------------------------------------------------
//
// The third region of a product page. Inspecting a row used to mean leaving
// the list for the detail route — losing the filter, the sort and the scroll
// position — and only five products implement Driller at all, so for every
// other one a row could not be inspected in any way.
//
// It shows what the provider actually returned for that row and nothing else:
// a panel that displayed a field the backend does not hold would be inventing
// the answer to the question it exists to answer.

const INFO_OPEN_KEY = "cloudburrow.infopanel";
let INFO_TOGGLE = null;

const infoPanelOpen = () => readStored(INFO_OPEN_KEY, false) === true;

function infoPanelHost() {
  const panel = document.getElementById("info-panel");
  if (panel && !panel.dataset.wired) {
    panel.dataset.wired = "true";
    panel.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      closeInfoPanel();
    });
  }
  return panel;
}

function closeInfoPanel() {
  writeStored(INFO_OPEN_KEY, false);
  const panel = infoPanelHost();
  if (panel) panel.hidden = true;
  document.documentElement.removeAttribute("data-info");
  if (INFO_TOGGLE && INFO_TOGGLE.isConnected) {
    INFO_TOGGLE.setAttribute("aria-expanded", "false");
    INFO_TOGGLE.textContent = "Show info panel";
    INFO_TOGGLE.focus();
  }
}

// drawInfoPanel fills the panel from one row, or says nothing is selected.
function drawInfoPanel(item, columns, route, onDone) {
  const panel = infoPanelHost();
  if (!panel || panel.hidden) return;

  if (!item) {
    return setChildren(panel,
      el("div", { class: "info-empty" },
        el("h2", { text: "Select a resource" }),
        el("p", { class: "unavailable",
                  text: "Choose a row to see what this instance holds for it." })));
  }

  const caps = capabilityOf(route.service);
  const actions = [
    ...(item.actions || []).map((a) => ({
      label: a.label, destructive: a.destructive,
      run: () => runAction(route, item.name, a, onDone),
    })),
    ...(caps.delete ? [{
      label: "Delete", destructive: true,
      run: () => deleteResource(route, item.name, onDone),
    }] : []),
  ];

  setChildren(panel,
    el("div", { class: "info-head" },
      el("h2", { text: item.name }),
      el("button", { class: "icon-button", "aria-label": "Close info panel",
                     onclick: () => closeInfoPanel(),
                     html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>' })),
    item.status
      ? el("p", {}, el("span", { class: "status", "data-state": stateOf(item.status) },
          el("span", { text: item.status })))
      : null,
    // Only the columns the listing itself declares, so the panel can never
    // show a field the provider did not return.
    el("dl", { class: "info-fields" }, ...columns.flatMap((c) => {
      const value = (item.fields || {})[c];
      return value ? [el("dt", { text: c }), el("dd", { text: value })] : [];
    })),
    item.link
      ? el("p", {}, el("a", { href: item.link, text: "Open" }))
      : caps.detail
        ? el("p", {}, el("a", { href: detailHref(route, item.name), text: "Open" }))
        : null,
    actions.length
      ? el("div", { class: "info-actions" }, ...actions.map((a) =>
          el("button", {
            class: "secondary" + (a.destructive ? " danger" : ""),
            text: a.label, onclick: () => a.run(),
          })))
      : null);
}

// --- the query pane ---------------------------------------------------
//
// An editor over the transport at POST /api/query. Deliberately plain: no
// syntax highlighting, no autocomplete, no third-party editor component —
// the assets ship as they are written, and a CDN-loaded editor would break
// the one promise this console makes about working offline.
//
// What it does have is the backend's own error text, unchanged. A syntax
// error names the character; a read-only violation names the statement.
// Replacing either with "query failed" throws away the entire answer.

const QUERY_DRAFT_KEY = "cloudburrow.query";

function queryPane(route, segments, spec, onDone) {
  // A provider with no query language gets a form instead of a statement box.
  // Which one it is comes from the backend: "does this database have a query
  // language" is not a question the browser can answer, and a textarea over
  // Firestore would mean inventing a syntax nobody else accepts.
  if (spec && (spec.fields || []).length) {
    return queryFormPane(route, segments, spec, onDone);
  }
  const hint = spec && spec.hint;
  const draftKey = `${QUERY_DRAFT_KEY}.${route.service}.${segments.join("/")}`;

  const editor = el("textarea", {
    class: "query-editor mono", rows: "6", spellcheck: "false",
    "aria-label": "Statement",
    placeholder: "SELECT * FROM widgets LIMIT 10",
  });
  // The draft survives a navigation away and back, because losing a
  // half-written query to a misclick is the fastest way to stop using a
  // query pane. Per viewer and per resource; never sent anywhere.
  editor.value = readStored(draftKey, "");
  editor.addEventListener("input", () => writeStored(draftKey, editor.value));

  const results = el("div", { class: "query-results" });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const run = el("button", { class: "primary", text: "Run" });

  const execute = async () => {
    const statement = editor.value.trim();
    if (!statement) return;
    error.hidden = true;
    setBusy(run, true);
    const started = performance.now();
    try {
      const data = await send(
        `/api/query/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Statement: statement });
      const listing = data.listing || {};
      const took = Math.round(performance.now() - started);

      if (!(listing.items || []).length) {
        setChildren(results,
          el("p", { class: "unavailable",
                    text: `No rows. ${took} ms.` }));
      } else {
        setChildren(results);
        renderTableInto(results, [
          el("p", { class: "unavailable",
                    text: `${listing.items.length} row${listing.items.length === 1 ? "" : "s"} · ${took} ms` }),
        ], listing, listing.noun || "rows", () => execute(), route, {});
      }
      announce(`Query returned ${(listing.items || []).length} rows`);
    } catch (err) {
      // The database said this. Saying it again in our own words would be
      // replacing the answer with a summary of the answer.
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      setBusy(run, false);
    }
    if (onDone) onDone();
  };

  run.addEventListener("click", execute);
  // Ctrl/Cmd+Enter runs, which is what every query surface binds it to.
  editor.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      execute();
    }
  });

  return el("div", { class: "query-pane" },
    el("div", { class: "card" },
      editor,
      el("div", { class: "card-actions" },
        run,
        el("button", { class: "secondary", text: "Clear",
          onclick: () => { editor.value = ""; writeStored(draftKey, ""); setChildren(results); error.hidden = true; } }),
        el("span", { class: "unavailable", text: "⌘/Ctrl + Enter to run" })),
      hint ? el("p", { class: "unavailable", text: hint }) : null),
    error,
    results);
}

// queryFormPane builds a structured query from controls.
//
// Firestore, Datastore and Bigtable queries are structures, not text, so this
// reuses the create form's builder: the same validation, the same grouping, the
// same required marks. Results render with the same table as every other
// listing, so sorting and filtering work here without being written twice.
function queryFormPane(route, segments, spec, onDone) {
  const fields = buildCreateForm({ label: spec.label || "Run", fields: spec.fields });
  const results = el("div", { class: "query-results" });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const run = el("button", { class: "primary", type: "submit", text: spec.label || "Run" });

  const execute = async (e) => {
    if (e) e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) return;
    setBusy(run, true);
    const started = performance.now();
    try {
      const data = await send(
        `/api/query/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Path: segments, Values: fields.values() });
      const listing = data.listing || {};
      const took = Math.round(performance.now() - started);
      const count = (listing.items || []).length;
      if (!count) {
        setChildren(results, el("p", { class: "unavailable",
          text: `No rows matched. ${took} ms.` }));
      } else {
        setChildren(results);
        renderTableInto(results, [
          el("p", { class: "unavailable",
                    text: `${count} row${count === 1 ? "" : "s"} · ${took} ms` }),
        ], listing, listing.noun || "rows", () => execute(), route, {});
      }
      announce(`Query returned ${count} rows`);
    } catch (err) {
      // The database said this. Saying it again in our own words would be
      // replacing the answer with a summary of the answer.
      setChildren(results);
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      setBusy(run, false);
    }
    if (onDone) onDone();
  };

  return el("div", { class: "query-pane" },
    el("form", { class: "card query-form", novalidate: true, onsubmit: execute },
      ...fields.nodes,
      el("div", { class: "card-actions" }, run,
        el("button", { class: "secondary", type: "button", text: "Clear results",
          onclick: () => { setChildren(results); error.hidden = true; } }))),
    error,
    results);
}

// --- create form ------------------------------------------------------
//
// The form is described by the backend, so a field only appears when the
// service behind it can accept it.
//
// One builder serves both the dialog and the full-page form. Two renderings
// of one description would drift, and validation is the part that would drift
// silently — the half that still looked right while accepting what the API
// refuses.

function buildCreateForm(spec) {
  const entries = spec.fields.map((f) => {
    const id = `f-${f.name}`;
    const errorId = `e-${f.name}`;
    const helpId = f.help ? `h-${f.name}` : null;
    const isCheck = f.type === "checkbox";
    const isMap = f.type === "map";
    const isArea = f.type === "textarea" || isMap;

    // A textarea rather than an input wherever the value can hold newlines:
    // Enter inserts one instead of submitting the form, which is the whole
    // difference between a usable DDL box and a single-line one.
    const control = isArea
      ? el("textarea", { id, name: f.name, rows: isMap ? "4" : "5", required: f.required })
      : el("input", {
          id, name: f.name, type: f.type || "text", required: f.required,
          // `pattern` is only enforced on the text-like inputs. Attaching one
          // elsewhere would be a constraint nothing applies.
          pattern: isCheck || isArea ? null : (f.pattern || null),
        });
    if (isCheck) control.checked = f.default === "true";
    else control.value = f.default || "";
    if (helpId) control.setAttribute("aria-describedby", helpId);
    // An immutable field is shown so the operator can see which resource they
    // are editing, and refused so the form does not accept a change the API
    // will not apply. Disabled rather than hidden: hiding it would read as
    // though the resource did not have the property.
    if (f.immutable) {
      control.disabled = true;
      control.setAttribute("aria-readonly", "true");
    }

    const label = el("label", { for: id },
      el("span", { text: f.label }),
      // The marker is decorative: assistive technology reads the requirement
      // from the control's own `required`, so announcing "star" as well would
      // be saying it twice.
      f.required ? el("span", { class: "required-mark", "aria-hidden": "true", text: "*" }) : null);
    const help = f.help ? el("p", { id: helpId, class: "form-help", text: f.help }) : null;
    const error = el("p", { id: errorId, class: "form-field-error", hidden: true });

    const node = isCheck
      ? el("div", { class: "form-row is-check" },
          el("div", { class: "check-line" }, control, label), help, error)
      : el("div", { class: "form-row" }, label, control, help, error);

    return { field: f, control, node, error, helpId, errorId, isCheck };
  });

  // A map field is edited one "key=value" per line and submitted as the JSON
  // object the backend decodes. The lines are the editable form; the JSON is
  // the wire format, and the user should never have to write braces.
  const mapEntries = entries.filter((e) => e.field.type === "map");
  for (const entry of mapEntries) {
    entry.control.value = mapToLines(entry.field.default);
    entry.control.setAttribute("spellcheck", "false");
    const validate = () => {
      const bad = badMapLine(entry.control.value);
      // setCustomValidity is what makes checkValidity() agree with what the
      // field actually accepts, so one validation path covers both.
      entry.control.setCustomValidity(bad ? `Line ${bad.line} is not "key=value".` : "");
    };
    entry.control.addEventListener("input", validate);
    validate();
  }

  const message = ({ field, control }) => {
    if (control.validity.valueMissing) return `${field.label} is required.`;
    // The help text is the constraint in words, so a pattern failure says
    // what the rule is rather than that a rule exists.
    if (control.validity.patternMismatch) {
      return field.help || `${field.label} is not in the expected format.`;
    }
    return control.validationMessage || `${field.label} is not valid.`;
  };

  const markValid = (entry) => {
    entry.error.textContent = "";
    entry.error.hidden = true;
    entry.node.classList.remove("is-invalid");
    entry.control.removeAttribute("aria-invalid");
    if (entry.helpId) entry.control.setAttribute("aria-describedby", entry.helpId);
    else entry.control.removeAttribute("aria-describedby");
  };

  const markInvalid = (entry) => {
    entry.error.textContent = message(entry);
    entry.error.hidden = false;
    entry.node.classList.add("is-invalid");
    entry.control.setAttribute("aria-invalid", "true");
    entry.control.setAttribute("aria-describedby",
      [entry.helpId, entry.errorId].filter(Boolean).join(" "));
  };

  const check = (entry) => {
    if (entry.control.checkValidity()) { markValid(entry); return true; }
    markInvalid(entry);
    return false;
  };

  for (const entry of entries) {
    // Checked when the field is left, and again on every keystroke once it is
    // already marked — so a correction clears the error as soon as it is a
    // correction, rather than at the next submit.
    entry.control.addEventListener("blur", () => check(entry));
    const recheck = () => { if (entry.node.classList.contains("is-invalid")) check(entry); };
    entry.control.addEventListener("input", recheck);
    entry.control.addEventListener("change", recheck);
  }

  // Fields group under their section heading, in the order the backend gave
  // them. A form whose fields declare no section renders as one block, which
  // is what a two-field form should look like.
  const groups = [];
  for (const entry of entries) {
    const name = entry.field.section || "";
    const last = groups[groups.length - 1];
    if (last && last.name === name) last.entries.push(entry);
    else groups.push({ name, entries: [entry] });
  }
  const nodes = groups.flatMap((g) => g.name
    ? [el("section", { class: "form-section" },
        el("h3", { class: "form-section-title", text: g.name }),
        ...g.entries.map((e) => e.node))]
    : g.entries.map((e) => e.node));

  // Stated once, not once per field: the marker means nothing until something
  // says what it means.
  if (entries.some((e) => e.field.required)) {
    nodes.unshift(el("p", { class: "form-required-note" },
      el("span", { class: "required-mark", "aria-hidden": "true", text: "*" }),
      el("span", { text: " Required" })));
  }

  const defaultOf = (entry) => {
    if (entry.isCheck) return entry.field.default === "true";
    if (entry.field.type === "map") return mapToLines(entry.field.default);
    return entry.field.default || "";
  };
  const valueOf = (entry) => (entry.isCheck ? entry.control.checked : entry.control.value);

  return {
    nodes,
    focusFirst() { if (entries.length) entries[0].control.focus(); },
    // Anything the user changed away from what the form offered. A form
    // holding only its own defaults has nothing to lose.
    dirty() { return entries.some((e) => valueOf(e) !== defaultOf(e)); },
    validate() {
      let first = null;
      for (const entry of entries) {
        if (!check(entry) && !first) first = entry;
      }
      if (first) first.control.focus();
      return !first;
    },
    values() {
      // An immutable field is context, not input. Sending it back would ask
      // the API to set a value to what it already is, which some APIs accept
      // and others reject as an attempt to change an immutable field.
      return Object.fromEntries(entries
        .filter((e) => !e.field.immutable)
        .map((e) => {
          if (e.isCheck) return [e.field.name, String(e.control.checked)];
          if (e.field.type === "map") return [e.field.name, linesToMap(e.control.value)];
          return [e.field.name, e.control.value];
        }));
    },
  };
}

// mapToLines renders a map field's JSON value as one "key=value" per line.
function mapToLines(value) {
  if (!value) return "";
  let parsed;
  try {
    parsed = JSON.parse(value);
  } catch {
    // A value the backend sent that is not JSON is shown as-is rather than
    // silently replaced with nothing: losing the operator's data to a parse
    // failure is worse than showing them something odd.
    return value;
  }
  if (!parsed || typeof parsed !== "object") return value;
  return Object.entries(parsed).map(([k, v]) => `${k}=${v}`).join("\n");
}

// badMapLine returns the first line that is not "key=value", or null.
function badMapLine(text) {
  const lines = String(text).split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line) continue;
    const at = line.indexOf("=");
    // A key is required; an empty value is legal, because an empty label value
    // is legal.
    if (at <= 0) return { line: i + 1, text: line };
  }
  return null;
}

// linesToMap encodes "key=value" lines as the JSON object the backend parses.
function linesToMap(text) {
  const out = {};
  for (const raw of String(text).split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const at = line.indexOf("=");
    if (at <= 0) continue;
    // Only the first "=" splits, so a value may contain one.
    out[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return Object.keys(out).length ? JSON.stringify(out) : "";
}

// submitCreate posts the form and records the operation around it.
async function submitCreate(route, spec, values) {
  const op = recordOperation(`${spec.label} in ${route.title}`);
  try {
    const res = await send(
      `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}`,
      "POST", values);
    // The id is what lets the local entry and the server's record be
    // recognised as the same operation rather than shown twice.
    op.succeeded(res.name, res.operation);
    return res.name;
  } catch (err) {
    op.failed(err.message, err.operation);
    throw err;
  }
}

// startCreate opens the form wherever the backend says it belongs.
//
// The threshold is the server's, not the client's, so "long enough to deserve
// a page" has one definition rather than one per caller.
function startCreate(route, create, onDone) {
  if (create.page) return navigate(`${route.path}/create`);
  openCreateForm(route, create, onDone);
}

function openCreateForm(route, spec, onDone) {
  const fields = buildCreateForm(spec);
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const discard = el("div", { class: "discard-prompt", role: "alert", hidden: true });

  let submitting = false;
  let discarding = false;

  const { dialog, close } = openModal({
    labelledBy: "create-title",
    canClose: (reason) => {
      if (discarding) return true;
      // A request is in flight. Dismissing now would leave the operation
      // completing against a form that no longer exists, and the user with no
      // idea whether it happened.
      if (submitting) return false;
      if (!fields.dirty()) return true;
      // A stray click on the backdrop is not a decision to throw away typed
      // input, so it does nothing at all. Escape and Cancel are decisions, so
      // they ask.
      if (reason === "backdrop") return false;
      discard.hidden = false;
      discard.querySelector("button").focus();
      return false;
    },
  });

  setChildren(discard,
    el("span", { text: "Discard your changes?" }),
    el("button", { type: "button", class: "secondary", text: "Keep editing",
      onclick: () => { discard.hidden = true; fields.focusFirst(); } }),
    el("button", { type: "button", class: "secondary danger", text: "Discard",
      onclick: () => { discarding = true; close(); } }));

  const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                onclick: () => close() });
  const primary = el("button", { type: "submit", class: "primary", text: spec.label });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    // novalidate below, so this is the only validation there is and the
    // console owns the message rather than the browser.
    if (!fields.validate()) return;

    submitting = true;
    setBusy(primary, true);
    cancel.disabled = true;
    try {
      const name = await submitCreate(route, spec, fields.values());
      announce(`Created ${name}`);
      submitting = false;
      discarding = true;
      close();
      // The name is passed on so a caller can act on what was just made —
      // the project picker selects it. Existing callers ignore it.
      onDone(name);
    } catch (err) {
      // The banner carries the API's own rejection. A message about one
      // field belongs under that field, and is put there by validate().
      submitting = false;
      setBusy(primary, false);
      cancel.disabled = false;
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  dialog.append(el("form", { class: "modal-body", novalidate: true, onsubmit: submit },
    el("h2", { id: "create-title", text: spec.label }),
    error,
    ...fields.nodes,
    discard,
    el("div", { class: "modal-actions" }, cancel, primary)));
  fields.focusFirst();
}

// renderCreatePage draws the same form at an address of its own.
//
// A dialog has no URL: a refresh, a back button or a link shared with a
// colleague loses whatever was in it. A form long enough to be worth filling
// in is a form worth being able to return to.
async function renderCreatePage(view, route) {
  const target = route.of;
  const project = currentProject();
  const caps = capabilityOf(target.service);
  const back = new URL(target.path, location.origin);
  if (project) back.searchParams.set("project", project);
  const listHref = back.pathname + back.search;

  const header = [
    el("div", { class: "page-header" },
      el("nav", { class: "breadcrumb", "aria-label": "Breadcrumb" },
        el("a", { href: listHref, text: target.title }),
        el("span", { "aria-hidden": "true", text: "/" }),
        el("span", { text: caps.create ? caps.create.label : "Create" })),
      el("h1", { text: caps.create ? caps.create.label : "Create" }),
      el("p", { class: "subtitle", text: project ? `Project ${project}` : "All projects" })),
  ];

  if (!caps.create) {
    setChildren(view, ...header,
      emptyState(`${target.title} cannot be created from the console`,
        "The local instance does not offer this operation."));
    return;
  }

  const fields = buildCreateForm(caps.create);
  const error = el("p", { class: "form-error", role: "alert", hidden: true });
  const cancel = el("a", { class: "button secondary", href: listHref, text: "Cancel" });
  const primary = el("button", { type: "submit", class: "primary", text: caps.create.label });

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    if (!fields.validate()) return;

    setBusy(primary, true);
    try {
      const name = await submitCreate(target, caps.create, fields.values());
      notify(`Created ${name}`);
      announce(`Created ${name}`);
      navigate(target.path);
    } catch (err) {
      setBusy(primary, false);
      error.textContent = err.message;
      error.hidden = false;
    }
  };

  setChildren(view, ...header,
    el("form", { class: "create-page", novalidate: true, onsubmit: submit },
      error,
      ...fields.nodes,
      el("div", { class: "form-actions" }, cancel, primary)));
  fields.focusFirst();
  announce(`${caps.create.label} form`);
}

// currentProject is the scope every mutation is made in.
function currentProject() {
  return new URLSearchParams(location.search).get("project") || "";
}

// notify shows the outcome of something the user did.
//
// window.alert blocks the page, cannot be styled, cannot be dismissed by
// anything but a click, and says nothing at all when an action succeeds — so
// the only feedback the console gave was for failure, and it stopped the
// world to give it.
const NOTIFY_MS = 6000;

function notify(message, kind = "info") {
  let host = document.getElementById("snackbars");
  if (!host) {
    host = el("div", { id: "snackbars", class: "snackbars" });
    // Polite, not assertive: an outcome is worth announcing but not worth
    // interrupting whatever the user is reading.
    host.setAttribute("aria-live", "polite");
    document.body.append(host);
  }

  const bar = el("div", { class: `snackbar is-${kind}`, role: kind === "error" ? "alert" : null },
    el("span", { class: "snackbar-text", text: message }),
    el("button", {
      class: "snackbar-close", "aria-label": "Dismiss",
      onclick: () => bar.remove(),
      html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
    }));
  host.append(bar);

  // An error stays until dismissed: it is the one outcome the user may need
  // to read twice, or copy.
  if (kind !== "error") setTimeout(() => bar.remove(), NOTIFY_MS);
  announce(message);
  return bar;
}

// confirmDestructive asks for the name back before doing something
// irreversible.
//
// window.confirm cannot say which resource, cannot be styled and is one
// reflexive Enter away from deleting the wrong thing. Typing the name is the
// pattern the console this mirrors uses for exactly that reason: it makes the
// subject of the sentence something the user has to produce.
//
// It resolves once the dialog is gone, either way, so a caller can keep a row
// marked as busy for exactly as long as something is actually happening to it
// — which is not the same interval as "the dialog is open".
function confirmDestructive({ title, detail, confirmWord, onConfirm }) {
  return new Promise((settle) => {
    const error = el("p", { class: "form-error", role: "alert", hidden: true });
    const input = el("input", { type: "text", autocomplete: "off", id: "confirm-input" });

    let running = false;
    const { dialog, close } = openModal({
      labelledBy: "confirm-title",
      // Dismissing mid-delete would hide an operation that is still going.
      canClose: () => !running,
    });

    const confirm = el("button", { type: "submit", class: "primary danger", text: "Delete" });
    const cancel = el("button", { type: "button", class: "secondary", text: "Cancel",
                                  onclick: () => close() });

    const submit = async (e) => {
      e.preventDefault();
      if (input.value.trim() !== confirmWord) {
        error.textContent = `Type ${confirmWord} exactly to confirm.`;
        error.hidden = false;
        input.focus();
        return;
      }
      running = true;
      setBusy(confirm, true);
      cancel.disabled = true;
      try {
        await onConfirm();
        running = false;
        close();
      } catch (err) {
        running = false;
        setBusy(confirm, false);
        cancel.disabled = false;
        error.textContent = err.message;
        error.hidden = false;
      }
    };

    dialog.append(el("form", { class: "modal-body", onsubmit: submit },
      el("h2", { id: "confirm-title", text: title }),
      detail ? el("p", { class: "confirm-detail", text: detail }) : null,
      el("p", { text: "This cannot be undone." }),
      error,
      el("label", { for: "confirm-input" },
        el("span", { text: `Type ` }),
        el("strong", { text: confirmWord }),
        el("span", { text: ` to confirm` })),
      input,
      el("div", { class: "modal-actions" }, cancel, confirm)));

    dialog.addEventListener("cb-closed", () => settle());
    input.focus();
  });
}

// overflowMenu puts a row's actions behind one control.
function overflowMenu(actions, name) {
  const menu = el("div", { class: "overflow-menu", hidden: true, role: "menu" },
    ...actions.map((a) =>
      el("button", {
        class: "overflow-item" + (a.destructive ? " is-destructive" : ""),
        role: "menuitem", text: a.label,
        onclick: () => { menu.hidden = true; a.run(); },
      })));

  const button = el("button", {
    class: "icon-button overflow-trigger", "aria-label": `Actions for ${name}`,
    "aria-haspopup": "menu", "aria-expanded": "false",
    onclick: (e) => {
      e.stopPropagation();
      // One menu at a time, or two rows' actions sit on screen together and
      // it stops being obvious which row you are acting on.
      for (const other of document.querySelectorAll(".overflow-menu")) {
        if (other !== menu) other.hidden = true;
      }
      menu.hidden = !menu.hidden;
      button.setAttribute("aria-expanded", menu.hidden ? "false" : "true");
    },
    html: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="5" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="12" cy="19" r="1.6"/></svg>',
  });

  document.addEventListener("click", () => {
    menu.hidden = true;
    button.setAttribute("aria-expanded", "false");
  });

  return el("div", { class: "overflow" }, button, menu);
}

// A row that is not on screen — the project picker's delete, say — has
// nowhere to show progress, so the handle does nothing.
const NO_ROW = { start() {}, end() {} };

async function deleteResource(route, name, onDone, row = NO_ROW) {
  // Awaited, so the caller knows when the row stops being busy. The interval
  // that matters starts when the request goes out, not when the dialog opens:
  // a row marked busy while someone reads a confirmation would be lying.
  await confirmDestructive({
    title: `Delete ${name}?`,
    confirmWord: name,
    onConfirm: async () => {
      const op = recordOperation(`Delete ${name}`);
      row.start();
      try {
        const res = await send(
          `/api/resources/${route.service}?project=${encodeURIComponent(currentProject())}` +
          `&name=${encodeURIComponent(name)}`, "DELETE");
        op.succeeded("", res.operation);
        row.end();
        notify(`Deleted ${name}`);
        onDone();
      } catch (err) {
        op.failed(err.message, err.operation);
        row.end();
        notify(`Could not delete ${name}: ${err.message}`, "error");
        throw err;
      }
    },
  });
}

// runAction performs one action on a resource.
//
// `target` is either a name, which is how a list row addresses itself, or an
// array of path segments, which is how a detail page addresses a resource
// inside a resource. The backend distinguishes the two, so the client does
// not have to flatten one into the other.
async function runAction(route, target, action, onDone, row = NO_ROW) {
  const path = Array.isArray(target) ? target : null;
  const name = path ? path[path.length - 1] : target;
  // An action that declares fields needs a value before it can be performed,
  // so it asks for one rather than firing on click. The form is the create
  // form: one implementation, so an action's inputs validate the way every
  // other input does.
  if ((action.fields || []).length) {
    return openActionForm(route, path || [name], action, onDone);
  }
  const body = path
    ? { Path: path, Action: action.id }
    : { Name: name, Action: action.id };
  const apply = async () => {
    const op = recordOperation(`${action.label} ${name}`);
    row.start();
    try {
      const res = await send(
        `/api/actions/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", body);
      op.succeeded("", res.operation);
      row.end();
      notify(`${action.label} applied to ${name}`);
      onDone();
    } catch (err) {
      op.failed(err.message, err.operation);
      row.end();
      notify(`${action.label} failed for ${name}: ${err.message}`, "error");
      throw err;
    }
  };

  if (!action.destructive) {
    try { await apply(); } catch { /* reported by notify */ }
    return;
  }
  await confirmDestructive({
    title: `${action.label} ${name}?`,
    confirmWord: name,
    onConfirm: apply,
  });
}

// stateOf classifies a status word into a colour.
//
// It used to be three exact-match lists, so every word nobody had thought of
// rendered grey — and the words nobody had thought of were the ones that
// matter. A pod in CrashLoopBackOff or ImagePullBackOff, a Warning event, a
// container that exited non-zero: each of those came back "" and was drawn as
// neutral, which is the console saying nothing is wrong while something is.
//
// The suffix rules are what make it hold up against a word that has not been
// invented yet. Kubernetes names its container-waiting reasons consistently —
// anything ending in BackOff or beginning with Err or Failed is a problem —
// and a classifier that only knows today's list will be wrong again tomorrow.
function stateOf(status) {
  if (!status) return "";
  const s = String(status).toLowerCase();

  if (["ready", "running", "enabled", "active", "succeeded", "completed",
       "normal", "true"].includes(s)) return "ok";

  if (["failed", "error", "destroyed", "evicted", "oomkilled",
       "deadlineexceeded"].includes(s)) return "error";
  // CrashLoopBackOff, ImagePullBackOff, ErrImagePull,
  // CreateContainerConfigError, InvalidImageName — named by pattern rather
  // than listed, because the list is the cluster's to extend, not ours.
  if (s.endsWith("backoff") || s.startsWith("err") || s.startsWith("failed") ||
      s.endsWith("error")) return "error";

  if (["pending", "paused", "disabled", "unknown", "warning", "terminating",
       "containercreating", "podinitializing", "notready"].includes(s)) return "warn";

  return "";
}

function notFound(view, path) {
  setChildren(view, 
    el("h1", { text: "Page not found" }),
    emptyState("No such screen", `${path} does not exist in this console.`)
  );
}

// --- router ----------------------------------------------------------

// route dispatches, and does not let a screen fail silently.
//
// dispatch() returns the render's promise, which nobody used to await: a
// throw inside a render function left the skeleton on screen permanently.
function route() {
  const view = document.getElementById("view");
  const title = () => {
    const match = ROUTES.find((r) => r.path === location.pathname);
    return match ? match.title : "This screen";
  };
  try {
    const pending = dispatch(view);
    if (pending && typeof pending.catch === "function") {
      pending.then(syncStickyOffsets, (err) => screenFailed(err, title()));
    } else {
      syncStickyOffsets();
    }
    return pending;
  } catch (err) {
    screenFailed(err, title());
    return undefined;
  }
}

// routeFor resolves an address to a screen and, past it, a resource path.
//
// docs/console-parity.md section 5 promises /storage/browser/{bucket} and
// /run/{service}: a resource lives at its own address, not in a query
// parameter. The exact match is tried first so /run/create stays the create
// form rather than a service called "create".
function routeFor(pathname) {
  const exact = ROUTES.find((r) => r.path === pathname);
  if (exact) return { route: exact, path: [] };

  // The longest prefix wins, so /kubernetes/workloads beats /kubernetes.
  let best = null;
  for (const r of ROUTES) {
    if (!r.service || r.screen === "create") continue;
    if (pathname === r.path || !pathname.startsWith(r.path + "/")) continue;
    if (!best || r.path.length > best.path.length) best = r;
  }
  if (!best) return { route: null, path: [] };
  const rest = pathname.slice(best.path.length + 1);
  // Each segment was encoded on the way out, so an object key containing a
  // slash survives as its own segment rather than splitting into two.
  return { route: best, path: rest.split("/").filter(Boolean).map(decodeURIComponent) };
}

function dispatch(view) {
  markCurrent();
  drawProductNav();

  const { route: match, path: resourcePath } = routeFor(location.pathname);
  document.title = match ? `${match.title} — CloudBurrow` : "CloudBurrow Console";

  stopStream();
  stopMetrics();
  stopMonitoring();
  stopListPoll();
  METRICS_TICK = null;
  stopActivityPolling();
  REVEAL_SUSPENDED = false;
  if (!match) return notFound(view, location.pathname);
  if (match.screen === "search") return renderSearch(view);
  if (match.screen === "playground") return renderPlayground(view);
  if (match.screen === "monitoring") return renderMonitoring(view);
  if (match.screen === "logs") return renderLogs(view);
  if (match.screen === "activity") return renderActivity(view);
  if (match.screen === "create") return renderCreatePage(view, match);
  if (match.screen === "products") return renderProducts(view);
  if (!match.service) return renderDashboard(view);
  if (resourcePath.length) return renderDetail(view, match, resourcePath);
  // The old address still works, so a link someone saved keeps resolving.
  const legacy = new URLSearchParams(location.search).get("resource");
  if (legacy) return renderDetail(view, match, [legacy]);
  return renderList(view, match);
}

// syncStickyOffsets measures the pinned blocks so the ones below them know
// where to stop.
//
// The header block's height is not a constant: a list screen has a title and
// a subtitle, a detail screen adds a breadcrumb and may add a tab strip.
// Guessing it leaves either a gap under the toolbar or a bar hidden behind
// the header, and the header is the thing that moves.
function syncStickyOffsets() {
  const root = document.documentElement;
  const height = (sel) => {
    const node = document.querySelector(sel);
    return node ? `${Math.round(node.getBoundingClientRect().height)}px` : "0px";
  };
  root.style.setProperty("--page-header-h", height("#view .page-header"));
  root.style.setProperty("--action-bar-h", height("#view .action-bar"));
}

// drawProductNav lists the pages of the product the current screen belongs to.
//
// The drawer answers "which products exist"; this answers "which pages does
// this product have", which is how somebody who uses the real console looks
// for something — product first, page second. A product with one page shows
// nothing, because a list of one is not navigation.
function drawProductNav() {
  const host = document.getElementById("product-nav");
  if (!host) return;

  const here = ROUTES.find((r) => r.path === location.pathname && r.section);
  const pages = here ? pagesOf(productKey(here)) : [];
  if (pages.length < 2) {
    host.hidden = true;
    setChildren(host);
    return;
  }

  host.hidden = false;
  setChildren(host,
    el("span", { class: "product-nav-title", text: productTitle(here) }),
    el("ul", {}, ...pages.map((page) =>
      el("li", {},
        el("a", {
          href: page.path + scopeSearch(),
          class: page.path === location.pathname ? "is-current" : null,
          "aria-current": page.path === location.pathname ? "page" : null,
          text: page.title,
        })))));
}

// navigate goes to a path inside the console, keeping the project scope.
//
// The same thing a link click does, for the places where the control is a
// button because it is an action rather than a destination.
function navigate(path) {
  const url = new URL(path, location.origin);
  const project = currentProject();
  if (project) url.searchParams.set("project", project);
  history.pushState({}, "", url);
  // Focus lands on the main region first and the screen is drawn after, so a
  // screen that wants focus somewhere specific — a form's first field — is
  // the last one to move it rather than the first.
  document.getElementById("main").focus();
  route();
}

function initRouting() {
  document.addEventListener("click", (e) => {
    const link = e.target.closest("a[href]");
    if (!link) return;
    const url = new URL(link.href, location.origin);
    if (url.origin !== location.origin) return;
    e.preventDefault();
    history.pushState({}, "", url);
    document.getElementById("main").focus();
    route();
  });
  window.addEventListener("popstate", route);
}

// The project picker.
//
// Projects come from the registry rather than from scanning resources that
// happen to exist. Scanning could not show a project with nothing in it, and
// could not offer to make one — so the console could never answer "which
// projects are there?" or "give me a new one".
async function initProjects() {
  const button = document.getElementById("project-button");
  const panel = document.getElementById("project-panel");
  const current = document.getElementById("project-current");
  const list = document.getElementById("project-list");
  const filter = document.getElementById("project-filter");
  const newButton = document.getElementById("project-new");

  let selected = new URLSearchParams(location.search).get("project") || "";
  if (!selected && DEFAULT_PROJECT) {
    selected = DEFAULT_PROJECT;
    const url = new URL(location.href);
    url.searchParams.set("project", selected);
    history.replaceState({}, "", url);
  }

  const closePicker = () => {
    hideOverlay(panel);
    button.setAttribute("aria-expanded", "false");
  };

  const select = (id) => {
    const url = new URL(location.href);
    if (id) url.searchParams.set("project", id);
    else url.searchParams.delete("project");
    history.pushState({}, "", url);
    selected = id;
    current.textContent = id || "All projects";
    closePicker();
    route();
  };

  let projects = [];
  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    const shown = projects.filter((p) =>
      !q || p.id.toLowerCase().includes(q) || (p.name || "").toLowerCase().includes(q));

    setChildren(list,
      el("li", {},
        el("button", {
          class: "picker-item" + (selected === "" ? " is-selected" : ""),
          onclick: () => select(""),
        },
          el("span", { class: "picker-item-name", text: "All projects" }),
          el("span", { class: "picker-item-id", text: "no project scope" }))),
      ...shown.map((p) =>
        el("li", {},
          el("button", {
            class: "picker-item" + (selected === p.id ? " is-selected" : ""),
            onclick: () => select(p.id),
          },
            el("span", { class: "picker-item-name", text: p.name || p.id }),
            el("span", { class: "picker-item-id", text: p.id })))));

    if (!shown.length && q) {
      list.append(el("li", {}, el("p", { class: "panel-empty", text: "No matching projects." })));
    }
  };

  const load = async () => {
    try {
      const data = await api("/api/resources/projects");
      projects = (data.items || []).map((i) => ({
        id: i.name, name: (i.fields || {}).Name || i.name,
      }));
    } catch {
      // The picker still shows what is selected; it simply cannot offer
      // alternatives, which is better than showing none at all.
      projects = selected ? [{ id: selected, name: selected }] : [];
    }
    draw();
  };

  current.textContent = selected || "All projects";
  filter.addEventListener("input", draw);

  button.addEventListener("click", async () => {
    if (overlayOpen(panel)) return closePicker();
    await load();
    showOverlay(panel);
    button.setAttribute("aria-expanded", "true");
    filter.focus();
  });
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { closePicker(); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    if (overlayOpen(panel) && !panel.contains(e.target) && !button.contains(e.target)) {
      closePicker();
    }
  });

  newButton.addEventListener("click", () => {
    closePicker();
    const route = ROUTES.find((r) => r.service === "projects");
    const caps = capabilityOf("projects");
    if (!route || !caps.create) {
      announce("Creating projects is not available");
      return;
    }
    // The same form the Resource Manager screen uses, so there is one
    // definition of what a project needs and one set of rules.
    openCreateForm(route, caps.create, async (created) => {
      await load();
      select(created || selected);
    });
  });

  await load();
}

// The toolbar search looks across every service.
//
// It used to copy its text into whatever filter happened to be on screen,
// which meant the most prominent control in the console could only narrow the
// page already open — and found nothing at all on a screen without a table.
function initSearch() {
  const search = document.getElementById("search");
  search.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    const q = search.value.trim();
    if (!q) return;
    const url = new URL("/search", location.origin);
    url.searchParams.set("q", q);
    const project = new URLSearchParams(location.search).get("project");
    if (project) url.searchParams.set("project", project);
    history.pushState({}, "", url);
    route();
  });

  // "/" focuses search, as it does in most consoles, but never while the
  // user is already typing somewhere.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
    // The target of a key event is not always an Element — it can be the
    // document itself — and calling matches() on one that is not throws
    // inside a global handler.
    const t = e.target;
    if (t instanceof Element && (t.matches("input, textarea, select") || t.isContentEditable)) return;
    e.preventDefault();
    search.focus();
    search.select();
  });
}

async function renderSearch(view) {
  const params = new URLSearchParams(location.search);
  const q = params.get("q") || "";
  const project = params.get("project") || "";

  const header = el("div", { class: "page-header" },
    el("h1", { text: "Search results" }),
    el("p", { class: "subtitle", text: q ? `for “${q}”` : "Type a query in the toolbar." }));

  document.getElementById("search").value = q;
  if (!q) return setChildren(view, header);

  setChildren(view, header, loadingState(3));

  let data;
  try {
    data = await api(`/api/search?q=${encodeURIComponent(q)}&project=${encodeURIComponent(project)}`);
  } catch (err) {
    return setChildren(view, header,
      errorState("Search failed", String(err.message), () => renderSearch(view)));
  }

  // A hit links to the thing it found, not to the screen that lists it.
  //
  // Finding a bucket among two hundred and being dropped on the bucket list is
  // asking the user to search twice. Where the provider can open a row, the
  // result goes straight to it.
  const linkFor = (hit) => {
    const r = ROUTES.find((x) => x.service === hit.service);
    if (!r) return "/";
    const caps = capabilityOf(hit.service);
    return caps.detail ? detailHref(r, hit.name) : r.path + scopeSearch();
  };

  // Grouped by service, because "where is it" is half of what a search
  // across services is being asked.
  const groups = new Map();
  for (const hit of data.hits || []) {
    if (!groups.has(hit.title)) groups.set(hit.title, []);
    groups.get(hit.title).push(hit);
  }

  const blocks = [...groups.entries()].map(([title, hits]) =>
    el("div", { class: "card" },
      el("h2", { text: `${title} (${hits.length})` }),
      el("ul", { class: "search-hits" },
        ...hits.map((h) =>
          el("li", {},
            el("a", { href: linkFor(h), class: "search-hit" },
              el("span", { class: "search-hit-name", text: h.name }),
              h.detail ? el("span", { class: "search-hit-detail", text: h.detail }) : null,
              h.status
                ? el("span", { class: "status", "data-state": stateOf(h.status) },
                    el("span", { text: h.status }))
                : null))))));

  // Products and pages match too. A console's search box is the fastest way to
  // reach a screen, and one that only looked at resource names could not
  // answer "where is Cloud Tasks" — the question a newcomer asks first.
  const needle = q.toLowerCase();
  const screens = ROUTES.filter((r) =>
    r.title.toLowerCase().includes(needle) ||
    (r.section || "").toLowerCase().includes(needle));

  const failed = Object.entries(data.failed || {});
  const screenBlock = screens.length
    ? el("div", { class: "card" },
        el("h2", { text: `Products and pages (${screens.length})` }),
        el("ul", { class: "search-hits" },
          ...screens.map((r) =>
            el("li", {},
              el("a", { href: r.path + scopeSearch(), class: "search-hit" },
                el("span", { class: "search-hit-name", text: r.title }),
                r.section
                  ? el("span", { class: "search-hit-detail", text: r.section })
                  : null)))))
    : null;

  setChildren(view, header,
    el("p", { class: "subtitle",
              text: `${(data.hits || []).length} resource(s) across ${data.searched} service(s)` +
                    (screens.length ? `, ${screens.length} product(s) or page(s)` : "") +
                    (data.truncated ? " — more exist than are shown" : "") }),
    // A service that could not be searched is named. Otherwise "no results"
    // would be indistinguishable from "could not look".
    failed.length
      ? el("div", { class: "card" },
          el("h2", { text: "Not searched" }),
          el("ul", { class: "search-hits" },
            ...failed.map(([name, why]) =>
              el("li", {}, el("span", { class: "unavailable", text: `${name}: ${why}` })))))
      : null,
    blocks.length || screenBlock
      ? el("div", { class: "cards" }, screenBlock, ...blocks)
      : el("div", { class: "state" },
          el("h2", { text: "No matches" }),
          el("p", { text: `Nothing matching “${q}” in the services that answered.` })));

  announce(`${(data.hits || []).length} search results`);
}

async function main() {
  installFailureSurfaces();
  installVisibilityPause();
  // A resize changes the header's height — a wrapped title, a tab strip that
  // gains a row — so the offsets are remeasured rather than fixed at render.
  window.addEventListener("resize", syncStickyOffsets);
  initTheme();
  initPanel("settings", "settings-panel");
  initPanel("account", "account-panel");
  // Opening the bell is what "seen" means, and it is also when the panel is
  // worth the round trip.
  initPanel("notifications", "notifications-panel", {
    onOpen: () => { refreshOperations().then(markOperationsSeen); },
  });
  initNavToggle();
  initRouting();
  initSearch();
  initSearchToggle();

  // Neither failure is fatal, but neither is discarded: a console that starts
  // with an empty navigation and says nothing about why is indistinguishable
  // from one that has no services.
  try {
    SERVICES = (await api("/api/services")).services || [];
  } catch (err) {
    notify(`The service list could not be read: ${err.message}`, "error");
  }
  try {
    DEFAULT_PROJECT = (await api("/api/status")).defaultProject || "";
  } catch (err) {
    notify(`The instance status could not be read: ${err.message}`, "error");
  }
  buildNav(SERVICES);

  // The panel is populated before it is ever opened, so a reload does not
  // erase the record of what just happened. What is already there at load is
  // marked seen, or the badge would announce the whole history every time the
  // page is refreshed.
  await refreshOperations();
  markOperationsSeen();
  // The relative times go stale on their own; nothing else would move them.
  setInterval(() => {
    const panel = document.getElementById("notifications-panel");
    if (panel && overlayOpen(panel)) renderOperations();
  }, 30000);

  // The project is resolved before the first screen renders, so a per-project
  // screen is not painted once with no project and again with one.
  await initProjects();
  route();
}

document.addEventListener("DOMContentLoaded", main);

// --- Monitoring -------------------------------------------------------
//
// Charts over the history the instance retained, and nothing else. Every
// series here is a series this cluster demonstrably holds: node CPU, node
// memory and the pod count, read from the kubelet. What is deliberately
// absent is as much the point — per-request latency, cost, quota and SLO data
// do not exist locally, and a chart of them would be invented.

let MONITORING_TIMER = null;
const MONITORING_REDRAW_MS = 5000;

function stopMonitoring() {
  if (MONITORING_TIMER) { clearInterval(MONITORING_TIMER); MONITORING_TIMER = null; }
}

async function renderMonitoring(view) {
  const header = [pageHeader("Monitoring",
    "Charts over the readings this instance has taken since it started.")];
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(3, { what: "metric history", onCancel: () => cancel.abort() }));

  const draw = async () => {
    let data;
    try {
      data = await api("/api/metrics/series", { signal: cancel.signal });
    } catch (err) {
      return setChildren(view, ...header,
        isCancelled(err)
          ? cancelledState("Monitoring not loaded", () => renderMonitoring(view))
          : errorState("Monitoring unavailable", String(err.message),
                       () => renderMonitoring(view)));
    }

    if (data.unavailable) {
      return setChildren(view, ...header, emptyState("No history", data.unavailable));
    }

    const samples = data.samples || [];
    const latest = [...samples].reverse().find((s) => !s.unavailable && (s.nodes || []).length);
    const node = latest ? latest.nodes[0] : null;

    // What the window actually covers, said rather than implied. An instance
    // up for thirty seconds shows thirty seconds.
    const started = data.startedAt ? new Date(data.startedAt) : null;
    const covered = started ? `since ${relativeTime(started)}` : "";
    const every = `one reading every ${data.intervalSeconds}s`;
    const foot = `${samples.length} readings, ${every}${covered ? ", " + covered : ""} · ` +
                 `${data.retention || "in memory only"}`;

    const gaps = samples.filter((s) => s.unavailable).length;

    setChildren(view, ...header,
      gaps
        ? el("p", { class: "unavailable",
            text: `${gaps} of ${samples.length} readings could not be taken and are drawn as gaps.` })
        : null,
      el("div", { class: "charts" },
        chartCard("Node CPU", seriesPoints(samples, (n) => n.cpuUsedCores), {
          label: "Node CPU cores used over the retained window",
          max: node ? node.cpuCapacityCores : undefined,
          current: node ? `${node.cpuUsedCores.toFixed(3)} of ${node.cpuCapacityCores} vCPU` : "",
          foot: `Kubelet instantaneous usage · ${foot}`,
        }),
        chartCard("Node memory", seriesPoints(samples, (n) => n.memoryUsedBytes), {
          label: "Node memory working set over the retained window",
          max: node ? node.memoryTotalBytes : undefined,
          current: node ? `${formatBytes(node.memoryUsedBytes)} of ${formatBytes(node.memoryTotalBytes)}` : "",
          foot: `Kubelet working set · ${foot}`,
        }),
        chartCard("Pods", seriesPoints(samples, (n) => n.pods), {
          label: "Pods running on the node over the retained window",
          current: node ? `${node.pods} pods` : "",
          foot: `Counted by the kubelet · ${foot}`,
        })),
      // Absence, stated. The alternative is a reader assuming these charts
      // are missing rather than impossible.
      el("div", { class: "card" },
        el("h2", { text: "Not charted here" }),
        el("p", { class: "unavailable", text:
          "Request count and latency need Knative's queue-proxy metrics, which " +
          "are off in this instance (#181). Cost, quota and SLO data do not " +
          "exist locally at all and are not approximated." })));
  };

  await draw();
  stopMonitoring();
  // The same interval the server samples at: drawing faster than the data
  // changes is motion without information.
  // The server samples on its own clock; redrawing faster than that is
  // motion without information.
  MONITORING_TIMER = setInterval(draw, MONITORING_REDRAW_MS);
}

// --- Logs Explorer ----------------------------------------------------
//
// Live by default, bounded, and pausable. A log view that cannot be paused is
// unusable the moment something interesting scrolls past.

let STREAM = null;
let METRICS_TIMER = null;

// The list poll.
//
// Every screen backed by /api/resources or /api/detail re-reads on a bounded
// interval, through the table's own refresh so sort, filter, page and scroll
// survive it. Without this a list went quietly stale: the console showed a
// world that had stopped existing and said nothing about it.
let LIST_POLL_TIMER = null;
let LIST_POLL_TICK = null;
const LIST_POLL_MS = 15000;

function registerListPoll(tick) {
  stopListPoll();
  LIST_POLL_TICK = tick;
  if (!document.hidden) LIST_POLL_TIMER = setInterval(tick, LIST_POLL_MS);
}

function stopListPoll() {
  if (LIST_POLL_TIMER) { clearInterval(LIST_POLL_TIMER); LIST_POLL_TIMER = null; }
  LIST_POLL_TICK = null;
}

// METRICS_TICK is the current dashboard's poll, or null when no dashboard is
// on screen. Held so the visibility listener below can resume the one that
// belongs to the screen the user is actually looking at.
let METRICS_TICK = null;

function stopMetrics() {
  if (METRICS_TIMER) { clearInterval(METRICS_TIMER); METRICS_TIMER = null; }
}

// A hidden tab is polling a cluster nobody is looking at.
//
// Suspended rather than slowed, and refreshed on return, so the first thing
// the user sees on coming back is current rather than however old the tab is.
function installVisibilityPause() {
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      stopMetrics();
      if (LIST_POLL_TIMER) { clearInterval(LIST_POLL_TIMER); LIST_POLL_TIMER = null; }
      return;
    }
    // Back on screen: one immediate tick each, so the first thing the reader
    // sees is current rather than however old the tab is.
    if (METRICS_TICK && !METRICS_TIMER) {
      METRICS_TICK();
      METRICS_TIMER = setInterval(METRICS_TICK, 5000);
    }
    if (LIST_POLL_TICK && !LIST_POLL_TIMER) {
      LIST_POLL_TICK();
      LIST_POLL_TIMER = setInterval(LIST_POLL_TICK, LIST_POLL_MS);
    }
  });
}

// Activity refreshes itself while anything on it is still running.
//
// It used to fetch once, so an operation that was running when the screen
// opened stayed RUNNING on it forever — the screen most likely to be watched
// during a slow deploy was the one that never updated. Cleared on route
// change with the same discipline as the metrics timer.
let ACTIVITY_TIMER = null;
const ACTIVITY_POLL_MS = 3000;

function stopActivityPolling() {
  if (ACTIVITY_TIMER) { clearTimeout(ACTIVITY_TIMER); ACTIVITY_TIMER = null; }
}

function stopStream() {
  if (STREAM) { STREAM.close(); STREAM = null; }
}

const MAX_RENDERED_LINES = 1000;

// A stream that has said nothing at all for this long is treated as stalled.
//
// The server sends a keepalive every 20 seconds, so two missed heartbeats is
// the threshold. Before this the client could not tell a healthy idle stream
// from one whose socket was open and dead — and on the Logs screen those two
// states say opposite things about whether the application is running.
const STREAM_SILENCE_MS = 45000;

// The Resource column is what makes an unattributed entry legible: a line
// with no project still says which pod or service produced it.
const LOG_COLUMNS = ["Time", "Severity", "Source", "Resource", "Message"];

async function renderLogs(view) {
  const params = new URLSearchParams(location.search);
  const project = params.get("project") || "";
  // The toolbar's project was being forwarded as a hard filter, and a pod's
  // log line carries no project — the Pub/Sub emulator serves every project
  // from one container, so there is nothing to attribute it to. The result
  // was a Logs Explorer that showed nothing at all on an instance holding
  // hundreds of lines, which reads as "my application is silent".
  //
  // The scope is now the user's, stated on screen and carried in the URL.
  // All sources is the default, because the alternative is a screen that
  // hides the logs it exists to show.
  let scope = params.get("scope") === "project" ? "project" : "all";
  // Followed from a failed operation in Activity. The server has always
  // supported the filter; the client simply never read it, so the one path
  // built to explain a failure landed on the unfiltered stream of the whole
  // instance.
  let operation = params.get("operation") || "";

  const scopeSelect = el("select", { id: "log-scope", "aria-label": "Log scope" },
    el("option", { value: "all", text: "All sources" }),
    el("option", { value: "project", text: project ? `Project ${project}` : "This project",
                   disabled: project ? null : "disabled" }));
  scopeSelect.value = scope;

  const severity = el("select", { id: "severity", "aria-label": "Minimum severity" },
    ...["", "INFO", "WARNING", "ERROR"].map((v) =>
      el("option", { value: v, text: v || "All severities" })));
  const source = el("input", { class: "filter", type: "search", id: "source",
    placeholder: "Source, e.g. run/my-service", "aria-label": "Filter by source" });
  const contains = el("input", { class: "filter", type: "search", id: "contains",
    placeholder: "Message contains", "aria-label": "Filter by message text" });

  const pauseButton = el("button", { class: "secondary", text: "Pause" });
  const reconnectButton = el("button", { class: "secondary", text: "Reconnect",
                                         hidden: true, onclick: () => connect() });
  // A .status, like every other state in this console. A muted grey string
  // was the one state indicator on the page with no colour, on the screen
  // people open when something is already wrong.
  const status = el("span", { class: "status", "data-state": "warn" },
    el("span", { text: "connecting…" }));
  const chips = el("div", { class: "filter-chips" });
  const body = el("tbody");
  const table = el("table", {},
    el("thead", {}, el("tr", {},
      LOG_COLUMNS.map((c) => el("th", { scope: "col", text: c })))),
    body);

  let paused = false;
  let buffered = [];
  let attempt = 0;
  let rows = 0;
  let silenceTimer = null;
  let stalled = false;

  const setStatus = (text, state) => {
    status.setAttribute("data-state", state);
    setChildren(status, el("span", { text }));
  };

  // An empty table is a claim, and which claim depends on why it is empty.
  // "There are no logs" sends a developer to debug their own application;
  // "the stream is down" sends them here. The list screens have said this
  // properly for a while — this screen was the outlier.
  // How many entries the instance holds, and how many of them no project can
  // be claimed for. Fetched only when the table is empty, which is the one
  // moment the numbers explain anything.
  let census = null;

  const drawEmpty = () => {
    if (rows) return;
    const disconnected = stalled || attempt;
    const hiddenByScope = !disconnected && scope === "project" && census &&
      census.unattributed > 0;

    setChildren(body, el("tr", {},
      el("td", { colspan: String(LOG_COLUMNS.length) },
        el("div", { class: "state state-inline" },
          el("h2", { text: disconnected
            ? "The log stream is not connected"
            : hiddenByScope
              ? `No entries are attributed to project ${project}`
              : "No entries match these filters" }),
          el("p", { text: disconnected
            ? "Nothing can be shown until the stream is back."
            : hiddenByScope
              ? `${census.unattributed} of the ${census.held} entries this instance holds ` +
                "carry no project — a pod's log line usually cannot be attributed to one."
              : "Nothing has been logged that matches. Widen the filters, or wait." }),
          hiddenByScope
            ? el("button", { class: "primary", text: "Show all sources",
                             onclick: () => setScope("all") })
            : null))));
  };

  // Asked for only when the table is empty: the answer is what turns "nothing
  // here" into which kind of nothing.
  const takeCensus = async () => {
    if (rows || census) return;
    try {
      const data = await api("/api/logs?limit=1");
      census = { held: data.held || 0, unattributed: data.unattributed || 0 };
      drawEmpty();
    } catch { /* the stream's own state already says the instance is unreachable */ }
  };

  const setScope = (next) => {
    scope = next;
    scopeSelect.value = next;
    census = null;
    const url = new URL(location.href);
    if (next === "project") url.searchParams.set("scope", "project");
    else url.searchParams.delete("scope");
    history.replaceState({}, "", url);
    connect();
    announce(next === "project" ? `Scoped to project ${project}` : "Showing all sources");
  };
  scopeSelect.addEventListener("change", () => setScope(scopeSelect.value));

  const append = (entry) => {
    if (!rows) body.replaceChildren();
    rows++;
    const row = el("tr", {},
      el("td", { class: "mono", text: new Date(entry.timestamp).toLocaleTimeString() }),
      el("td", {}, el("span", { class: "status",
        "data-state": entry.severity === "ERROR" ? "error"
          : entry.severity === "WARNING" ? "warn" : "ok" },
        el("span", { text: entry.severity }))),
      el("td", { class: "mono", text: entry.source || "—" }),
      // Project and operation ride along in the title, so an entry that
      // carries them can be traced without a column per field.
      el("td", { class: "mono", text: entry.resource || "—",
                 title: [entry.project ? `project ${entry.project}` : "no project",
                         entry.operationId ? `operation ${entry.operationId}` : null]
                   .filter(Boolean).join(" · ") }),
      el("td", { class: "mono", text: entry.message }));
    body.append(row);
    // Bounded: an unbounded log view eventually becomes the reason the tab
    // stops responding.
    while (body.childElementCount > MAX_RENDERED_LINES) {
      body.firstElementChild.remove();
      rows--;
    }
  };

  // Any traffic at all — an entry or a heartbeat — means the stream is alive.
  const heard = () => {
    stalled = false;
    if (silenceTimer) clearTimeout(silenceTimer);
    silenceTimer = setTimeout(() => {
      stalled = true;
      setStatus("stalled — no data for 45s", "error");
      reconnectButton.hidden = false;
      drawEmpty();
    }, STREAM_SILENCE_MS);
  };

  const drawScope = () => {
    setChildren(chips, operation
      ? el("span", { class: "chip" },
          el("span", { text: `operation ${operation}` }),
          el("button", {
            class: "chip-clear", "aria-label": "Clear the operation filter",
            onclick: () => {
              operation = "";
              // The address changes with the filter, so the scope survives a
              // reload and a cleared scope is not re-applied by one.
              const url = new URL(location.href);
              url.searchParams.delete("operation");
              history.replaceState({}, "", url);
              drawScope();
              connect();
            },
            html: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
          }))
      : null);
  };

  pauseButton.addEventListener("click", () => {
    paused = !paused;
    pauseButton.textContent = paused ? "Resume" : "Pause";
    if (paused) {
      setStatus(`paused — ${buffered.length} buffered`, "warn");
      announce("Log stream paused");
    } else {
      // Resume shows what happened while paused rather than skipping it:
      // the lines you paused to read are usually next to the ones you need.
      for (const e of buffered) append(e);
      buffered = [];
      setStatus("streaming", "ok");
      announce("Log stream resumed");
    }
  });

  const connect = () => {
    stopStream();
    if (silenceTimer) clearTimeout(silenceTimer);
    body.replaceChildren();
    rows = 0;
    stalled = false;
    reconnectButton.hidden = true;
    drawEmpty();

    const query = new URLSearchParams();
    if (scope === "project" && project) query.set("project", project);
    if (operation) query.set("operation", operation);
    if (severity.value) query.set("severity", severity.value);
    if (source.value) query.set("source", source.value);
    if (contains.value) query.set("contains", contains.value);
    query.set("limit", "200");

    setStatus(attempt ? `reconnecting (attempt ${attempt})` : "connecting…", "warn");

    const stream = new EventSource(`/api/stream?${query}`);
    STREAM = stream;
    stream.addEventListener("open", () => {
      // The backlog arrives immediately after open; anything still empty a
      // moment later is genuinely empty and worth explaining.
      setTimeout(takeCensus, 500);
      attempt = 0;
      setStatus(paused ? `paused — ${buffered.length} buffered` : "streaming", "ok");
      heard();
    });
    stream.addEventListener("keepalive", heard);
    stream.addEventListener("log", (e) => {
      heard();
      let entry;
      try { entry = JSON.parse(e.data); } catch { return; }
      if (paused) {
        buffered.push(entry);
        if (buffered.length > MAX_RENDERED_LINES) buffered.shift();
        setStatus(`paused — ${buffered.length} buffered`, "warn");
        return;
      }
      append(entry);
    });
    stream.addEventListener("error", () => {
      // EventSource reconnects on its own and resumes from Last-Event-ID, so
      // this reports rather than rebuilds — except once the browser has
      // closed the connection for good, which is the one case it will not
      // recover from and the only one worth a button.
      if (stream.readyState === EventSource.CLOSED) {
        setStatus("disconnected", "error");
        reconnectButton.hidden = false;
      } else {
        attempt++;
        setStatus(`reconnecting (attempt ${attempt})`, "warn");
      }
      drawEmpty();
    });
  };

  for (const control of [severity, source, contains]) {
    control.addEventListener("change", connect);
  }

  drawScope();
  setChildren(view, 
    pageHeader("Logs Explorer",
      "Live from the local stack. Credentials are redacted before an entry is stored."),
    el("div", { class: "actions" }, scopeSelect, severity, source, contains,
       pauseButton, reconnectButton, status),
    chips,
    el("div", { class: "table-wrap" }, table));

  connect();
  announce(operation ? `Logs Explorer opened, scoped to operation ${operation}`
                     : "Logs Explorer opened");
}

// --- Activity ---------------------------------------------------------

// activityListing shapes operations as a listing, in one place, so the first
// render and every poll after it cannot disagree about the columns.
function activityListing(ops) {
  return {
    nameColumn: "Operation",
    columns: ["Started", "Kind", "Resource", "Detail"],
    noun: "operations",
    alwaysStatus: true,
    total: ops.length,
    items: ops.map((op) => ({
      name: op.id,
      status: op.state,
      // A failed operation's id links to its own logs: "it failed" without
      // the reason is the least useful thing a console can say.
      link: op.state === "FAILED"
        ? `/logs?operation=${encodeURIComponent(op.id)}` : "",
      fields: {
        Started: new Date(op.started).toLocaleTimeString(),
        Kind: op.kind,
        Resource: op.resource || "—",
        Detail: op.error || "—",
      },
    })),
  };
}

async function renderActivity(view) {
  const project = new URLSearchParams(location.search).get("project") || "";
  setChildren(view, 
    pageHeader("Activity", "Operations this console performed."),
    loadingState(4));

  let data;
  try {
    data = await api(`/api/operations?project=${encodeURIComponent(project)}`);
  } catch (err) {
    setChildren(view, pageHeader("Activity", "Operations this console performed."),
      errorState("Activity unavailable", String(err.message), () => renderActivity(view)));
    return;
  }

  const ops = data.operations || [];
  if (!ops.length) {
    setChildren(view, 
      pageHeader("Activity", "Operations this console performed."),
      emptyState("No operations yet",
        "Create or delete something in the console and it will appear here."));
    return;
  }

  const running = ops.filter((op) => op.state !== "SUCCEEDED" && op.state !== "FAILED").length;

  // Activity is the screen someone opens after something failed, so it is
  // where "only the failures" and "sort by time" matter most — and it has the
  // most rows, since every console mutation appends one. It used to hand-build
  // its own table and get none of that, which is exactly the drift the one
  // shared renderer exists to prevent.
  const listing = activityListing(ops);

  const header = [
    pageHeader("Activity", "Operations this console performed."),
    running
      ? el("div", { class: "actions" },
          el("span", { class: "status is-working" },
            spinner(), el("span", { text: `${running} still running` })))
      : null,
  ];
  // A refetch rather than a screen re-render: Activity is the screen most
  // likely to be watched while something is running, and re-rendering it
  // threw away a typed filter, a chosen sort and the caret on every tick.
  // The running count is updated in place on the same pass.
  const runningLabel = el("span", { class: "status is-working" },
    spinner(), el("span", { text: `${running} still running` }));
  if (!running) runningLabel.hidden = true;

  renderTableInto(view, [...header.slice(0, 1), el("div", { class: "actions" }, runningLabel)],
    listing, "operations",
    () => renderActivity(view), { path: "/activity", service: null, title: "Activity" },
    {
      refetch: async () => {
        const fresh = await api(`/api/operations?project=${encodeURIComponent(project)}`);
        const ops = fresh.operations || [];
        const still = ops.filter((op) => op.state !== "SUCCEEDED" && op.state !== "FAILED").length;
        runningLabel.hidden = still === 0;
        setChildren(runningLabel, spinner(),
          el("span", { text: `${still} still running` }));
        return activityListing(ops);
      },
    });
  announce(`${ops.length} operations`);

  // No separate Activity timer any more: the table's own poll re-reads the
  // operations through refetch, which is what keeps a typed filter, a chosen
  // sort and the caret across a tick. A screen-level re-render threw all
  // three away every three seconds, on the screen most likely to be watched
  // while something is running.
  stopActivityPolling();
}

// --- AI Playground ----------------------------------------------------
//
// Real inference through the same HTTP API the official SDK drives. The
// screen never talks to a model directly, so it cannot work while the API is
// broken, and it cannot show a result the API did not produce.
//
// History is held in memory for this page only. It is never written to
// storage and never leaves the browser, which is why there is no setting to
// turn that off: there is nothing to turn off.

// The screen's own name matches its route, its navigation entry and the
// browser tab. "AI Playground" in the heading and "Vertex AI Studio"
// everywhere else meant the tab disagreed with the page.
const PLAYGROUND_TITLE = "Vertex AI Studio";
const PLAYGROUND_SUBTITLE = "Real inference through the same HTTP API the official SDK drives.";

const PLAYGROUND_HISTORY_LIMIT = 20;
let PLAYGROUND_HISTORY = [];
let PLAYGROUND_ABORT = null;

function stopGeneration() {
  if (PLAYGROUND_ABORT) { PLAYGROUND_ABORT.abort(); PLAYGROUND_ABORT = null; }
}

async function renderPlayground(view) {
  stopGeneration();
  setChildren(view, pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE), loadingState(3));

  let status;
  try {
    status = await api("/api/ai/playground");
  } catch (err) {
    return setChildren(view, 
      pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
      errorState("Playground unavailable", String(err.message), () => renderPlayground(view)));
  }

  if (!status.configured) {
    return setChildren(view, 
      pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
      emptyState("Local AI is not configured", status.note || ""));
  }

  const header = el("div", { class: "card" },
    el("dl", {},
      el("dt", { text: "Model" }), el("dd", { class: "mono", text: status.model }),
      el("dt", { text: "Publisher" }), el("dd", { text: status.publisher || "unknown" }),
      el("dt", { text: "Endpoint" }), el("dd", { class: "mono", text: status.endpoint }),
      el("dt", { text: "Readiness" }),
      el("dd", {}, el("span", { class: "status", "data-state": status.ready ? "ok" : "error" },
        el("span", { text: status.ready ? "Ready" : "Not answering" })))));

  if (status.unavailable) {
    header.append(el("p", { class: "unavailable", text: status.unavailable }));
  }
  if (status.community) {
    // The requirement is explicit: Gemma results must not be labelled as
    // Gemini results. The provenance is stated on the screen that shows the
    // output, not only in the documentation.
    header.append(el("p", { class: "unavailable", text: status.note }));
  }

  const prompt = el("textarea", { id: "pg-prompt", rows: "4",
    placeholder: "Ask the local model something…", "aria-label": "Prompt" });
  const send = el("button", { text: "Run" });
  const cancel = el("button", { class: "secondary", text: "Cancel", disabled: "disabled" });
  const timing = el("span", { class: "unavailable", text: "" });
  const output = el("pre", { class: "mono pg-output", id: "pg-output", "aria-live": "polite" });
  const historyBody = el("tbody");

  const refused = el("details", { class: "card" },
    el("summary", { text: `Generation options are refused, not ignored (${status.refused.length})` }),
    el("p", { text:
      "This endpoint honours no generation options. The runtime applies none of them, and a " +
      "request carrying one is rejected rather than answered as though it applied. These are " +
      "refused:" }),
    el("p", { class: "mono", text: status.refused.join(", ") }));

  const renderHistory = () => {
    historyBody.replaceChildren();
    for (const h of PLAYGROUND_HISTORY) {
      historyBody.append(el("tr", {},
        el("td", { class: "mono", text: h.at }),
        el("td", { text: h.prompt.length > 60 ? h.prompt.slice(0, 60) + "…" : h.prompt }),
        el("td", { class: "mono", text: h.ttft }),
        el("td", { class: "mono", text: h.total }),
        el("td", {}, el("span", {
          class: "status",
          "data-state": h.ok ? "ok" : h.cancelled ? "warn" : "error",
        }, el("span", { text: h.ok ? "OK" : h.cancelled ? "Cancelled" : "Failed" })))));
    }
    if (!PLAYGROUND_HISTORY.length) {
      historyBody.append(el("tr", {},
        el("td", { colspan: "5", class: "unavailable", text: "No requests yet this session." })));
    }
  };

  const run = async () => {
    const text = prompt.value.trim();
    if (!text) { announce("A prompt is required"); prompt.focus(); return; }

    stopGeneration();
    output.textContent = "";
    timing.textContent = "running…";
    send.disabled = true;
    cancel.disabled = false;

    const controller = new AbortController();
    PLAYGROUND_ABORT = controller;
    const started = performance.now();
    let firstAt = null;
    let ok = true;
    let cancelled = false;

    try {
      const resp = await fetch("/api/ai/playground", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ prompt: text }),
        signal: controller.signal,
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error(errorMessageOf(body) || `HTTP ${resp.status}`);
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        // Server-sent events are separated by a blank line; a partial event
        // is kept in the buffer rather than parsed as truncated JSON.
        let split;
        while ((split = buffer.indexOf("\n\n")) !== -1) {
          const chunk = buffer.slice(0, split);
          buffer = buffer.slice(split + 2);
          for (const line of chunk.split("\n")) {
            if (!line.startsWith("data: ")) continue;
            let event;
            try { event = JSON.parse(line.slice(6)); } catch { continue; }
            if (event.error) { ok = false; throw new Error(event.error.message || "generation failed"); }
            const part = event.candidates?.[0]?.content?.parts?.[0]?.text;
            if (part) {
              if (firstAt === null) firstAt = performance.now();
              output.textContent += part;
            }
          }
        }
      }
    } catch (err) {
      ok = false;
      if (err.name === "AbortError") {
        // Cancelling is not a failure. Recording it as one would teach a
        // user to distrust the history, since every run they stopped would
        // read as something going wrong.
        cancelled = true;
        output.textContent += "\n[cancelled]";
      } else {
        output.textContent += `\n[error] ${err.message}`;
      }
    } finally {
      const total = performance.now() - started;
      const ttft = firstAt === null ? "—" : `${Math.round(firstAt - started)} ms`;
      timing.textContent = `first token ${ttft} · total ${Math.round(total)} ms`;
      send.disabled = false;
      cancel.disabled = true;
      PLAYGROUND_ABORT = null;

      PLAYGROUND_HISTORY.unshift({
        at: new Date().toLocaleTimeString(), prompt: text,
        ttft, total: `${Math.round(total)} ms`, ok, cancelled,
      });
      PLAYGROUND_HISTORY = PLAYGROUND_HISTORY.slice(0, PLAYGROUND_HISTORY_LIMIT);
      renderHistory();
    }
  };

  send.addEventListener("click", run);
  cancel.addEventListener("click", stopGeneration);

  renderHistory();
  setChildren(view, 
    pageHeader(PLAYGROUND_TITLE, PLAYGROUND_SUBTITLE),
    header,
    refused,
    el("div", { class: "card" },
      prompt,
      el("div", { class: "card-actions" }, send, cancel, timing),
      output),
    el("div", { class: "card" },
      el("h2", { text: "This session" }),
      el("p", { class: "unavailable", text:
        `Bounded to ${PLAYGROUND_HISTORY_LIMIT} entries, held in memory for this page only. ` +
        "Nothing is written to storage and nothing leaves the browser." }),
      el("table", {},
        el("thead", {}, el("tr", {},
          ["Time", "Prompt", "First token", "Total", "Result"].map((c) =>
            el("th", { scope: "col", text: c })))),
        historyBody)));
}

// errorMessageOf pulls the API's own message out of an error body, so the
// screen shows which field was refused rather than a status code.
function errorMessageOf(body) {
  try {
    const parsed = JSON.parse(body);
    return parsed.error?.message || parsed.error || "";
  } catch {
    return body;
  }
}
