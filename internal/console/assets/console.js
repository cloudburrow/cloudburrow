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

  { path: "/kubernetes/workloads",  service: "workloads",   title: "Workloads", section: "Containers" },
  { path: "/kubernetes/pods",       service: "pods",        title: "Pods",      section: "Containers" },
  { path: "/kubernetes/services",   service: "k8sservices", title: "Services",  section: "Containers" },
  { path: "/kubernetes/jobs",       service: "jobs",        title: "Jobs",      section: "Containers" },
  { path: "/kubernetes/events",     service: "events",      title: "Events",    section: "Containers" },

  { path: "/storage/browser", service: "storage", title: "Cloud Storage", section: "Storage" },

  { path: "/firestore", service: "firestore", title: "Firestore", section: "Databases" },
  { path: "/datastore", service: "datastore", title: "Datastore", section: "Databases" },
  { path: "/bigtable",  service: "bigtable",  title: "Bigtable",  section: "Databases" },
  { path: "/spanner",   service: "spanner",   title: "Spanner",   section: "Databases" },
  { path: "/cloudsql",  service: "cloudsql",  title: "Cloud SQL", section: "Databases" },

  { path: "/pubsub/topics", service: "pubsub", title: "Pub/Sub",     section: "Integration services" },
  { path: "/tasks/queues",  service: "tasks",  title: "Cloud Tasks", section: "Integration services" },

  { path: "/ai/models",     service: "ai",         title: "Vertex AI Model Garden", section: "AI and machine learning" },
  { path: "/ai/playground", service: "playground", screen: "playground",
    title: "Vertex AI Studio", section: "AI and machine learning" },

  { path: "/secrets", service: "secrets", title: "Secret Manager", section: "Security and identity" },

  { path: "/logs",     service: null, screen: "logs",     title: "Logs Explorer", section: "Operations" },
  { path: "/activity", service: null, screen: "activity", title: "Activity",      section: "Operations" },

  { path: "/projects", service: "projects", title: "Resource Manager", section: "Management tools" },

  { path: "/search", service: null, screen: "search", title: "Search results" },
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

function navLink(entry, onPinChange, nested = false, draggable = false) {
  const pinned = isPinned(entry.service);
  const pin = el("button", {
    class: "nav-pin" + (pinned ? " is-pinned" : ""),
    "aria-label": (pinned ? "Unpin " : "Pin ") + entry.title,
    "aria-pressed": pinned ? "true" : "false",
    title: pinned ? "Unpin" : "Pin",
    onclick: (e) => {
      e.preventDefault();
      e.stopPropagation();
      togglePinned(entry.service);
      onPinChange();
    },
  }, el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${PIN_ICON}</svg>` }));

  const row = el("li", {
    class: "nav-item" + (nested ? " nav-nested" : "") + (draggable ? " is-draggable" : ""),
    draggable: draggable ? "true" : null,
    "data-service": entry.service || null,
  },
    draggable ? dragHandle(entry, onPinChange) : null,
    el("a", {
      href: entry.path + scopeSearch(),
      "data-path": entry.path,
      "aria-label": entry.title, title: entry.title,
    },
      el("span", { class: "nav-icon" }, markFor(entry)),
      el("span", { class: "nav-label", text: entry.title })),
    entry.service ? pin : null);

  if (draggable) {
    row.addEventListener("dragstart", (e) => {
      e.dataTransfer.setData("text/plain", entry.service);
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
      if (moved && moved !== entry.service) {
        movePinned(moved, entry.service);
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

  const entries = ROUTES.filter((r) =>
    r.path !== "/search" && (!r.service || available.has(r.service) || !r.section));

  const dashboard = entries.find((e) => e.path === "/");
  const products = entries.filter((e) => e.path !== "/" && e.section);
  // In the order the developer arranged them, not the order they are declared.
  const byService = new Map(products.map((e) => [e.service, e]));
  const pinned = pinnedOrder().map((id) => byService.get(id)).filter(Boolean);

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
  const active = products.find((e) => e.path === location.pathname);
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
      onclick: () => {
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
    if (a.dataset.path === location.pathname) a.setAttribute("aria-current", "page");
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
    // Most products sit behind a collapsed category, so "view all" opens
    // every one rather than navigating somewhere that lists them again.
    const groups = openGroups();
    for (const section of Object.keys(CATEGORY_ICONS)) groups.add(section);
    writeStored(OPEN_GROUPS_KEY, [...groups]);
    // The catalogue itself has to be open, or expanding every category
    // inside a closed one shows nothing at all.
    writeStored(MORE_OPEN_KEY, true);
    buildNav(SERVICES);
    announce("All product categories expanded");
  });
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

  setChildren(view,
    el("div", { class: "page-header" },
      el("h1", { text: "Dashboard" }),
      el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." })),
    cards,
    el("div", { class: "cards", style: "margin-top:16px" },
      utilisation,
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
    ...(data.items.some((i) => i.status) ? ["Status"] : []),
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
      return el("tr", {
        class: [selected.has(item.name) ? "is-selected" : "", busy ? "is-operating" : ""]
          .filter(Boolean).join(" ") || null,
      }, ...cells);
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
      }));
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
  const refresh = async () => {
    if (!opts.refetch) return reload();
    try {
      const fresh = await opts.refetch();
      if (fresh && Array.isArray(fresh.items)) {
        data = fresh;
        // A selection may name rows that no longer exist.
        const names = new Set(data.items.map((i) => i.name));
        selected = new Set([...selected].filter((n) => names.has(n)));
        draw();
        return;
      }
    } catch (err) {
      notify(`Could not refresh: ${err.message}`, "error");
    }
    reload();
  };

  filter.addEventListener("input", () => { page = 0; draw(); });
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
          el("button", { class: "secondary", text: "Refresh", onclick: refresh }))
      : el("div", { class: "action-bar" },
          el("button", { class: "secondary", text: "Refresh", onclick: refresh })),
    el("div", { class: "filter-bar" }, filter),
    el("div", { class: "table-wrap" },
      el("table", {}, el("thead", {}, headRow), body)),
    footer);
}

// detailHref is the address of one row's contents.
function detailHref(route, name) {
  const url = new URL(route.path, location.origin);
  const project = new URLSearchParams(location.search).get("project");
  if (project) url.searchParams.set("project", project);
  url.searchParams.set("resource", name);
  return url.pathname + url.search;
}

// renderDetail shows what is inside one row.
//
// It draws the provider's listing with the same renderer as the list screen:
// one table implementation, so the two cannot drift apart, and sorting and
// filtering work here for free.
async function renderDetail(view, route, name) {
  const project = new URLSearchParams(location.search).get("project") || "";
  const back = new URL(route.path, location.origin);
  if (project) back.searchParams.set("project", project);

  const crumb = el("div", { class: "page-header" },
    // A breadcrumb, because a screen you can only leave with the browser
    // button is a screen you are stuck in.
    el("nav", { class: "breadcrumb", "aria-label": "Breadcrumb" },
      el("a", { href: back.pathname + back.search, text: route.title }),
      el("span", { "aria-hidden": "true", text: "/" }),
      el("span", { text: name })),
    el("h1", { text: name }),
    el("p", { class: "subtitle", text: project ? `Project ${project}` : "All projects" }));
  const header = [crumb];

  const path = `/api/detail/${route.service}?project=${encodeURIComponent(project)}` +
               `&name=${encodeURIComponent(name)}`;
  const cancel = new AbortController();
  setChildren(view, ...header,
    loadingState(5, { what: name, onCancel: () => cancel.abort() }));

  let data;
  try {
    data = await api(path, { signal: cancel.signal });
  } catch (err) {
    return setChildren(view, ...header,
      isCancelled(err)
        ? cancelledState(`${name} not loaded`, () => renderDetail(view, route, name))
        : errorState(`${name} unavailable`, String(err.message),
                     () => renderDetail(view, route, name)));
  }
  // The prompt is checked first, for the same reason it is on the list
  // screen: needing a project is a precondition, not a failure, and it must
  // never be rendered as one.
  if (data.prompt) {
    return setChildren(view, ...header, emptyState("Choose a project", data.prompt));
  }
  if (data.unavailable) {
    return setChildren(view, ...header,
      errorState(`${name} unavailable`, data.unavailable, () => renderDetail(view, route, name)));
  }

  const sections = data.sections || [];
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
    const list = section.listing || {};
    // A section that cannot be read says so inside its own panel. Rendering
    // it as an empty table would claim the resource holds nothing.
    if (list.unavailable) {
      return setChildren(panel,
        errorState(`${section.label} unavailable`, list.unavailable,
                   () => renderDetail(view, route, name)));
    }
    const noun = list.noun || section.label.toLowerCase();
    if (!(list.items || []).length) {
      return setChildren(panel, emptyState(`No ${noun}`, `${name} holds no ${noun} yet.`));
    }
    renderTableInto(panel, [], list, noun, () => renderDetail(view, route, name), route, {
      refetch: async () => {
        const fresh = await api(path);
        const same = (fresh.sections || []).find((sec) => sec.id === section.id);
        return same ? same.listing : null;
      },
    });
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

  panel.setAttribute("aria-labelledby", `tab-${sections[current].id}`);
  setChildren(view, ...header, summary, panel);
  drawPanel();
  announce(`${name} opened`);
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
    const isArea = f.type === "textarea";

    // A textarea rather than an input wherever the value can hold newlines:
    // Enter inserts one instead of submitting the form, which is the whole
    // difference between a usable DDL box and a single-line one.
    const control = isArea
      ? el("textarea", { id, name: f.name, rows: "5", required: f.required })
      : el("input", {
          id, name: f.name, type: f.type || "text", required: f.required,
          // `pattern` is only enforced on the text-like inputs. Attaching one
          // elsewhere would be a constraint nothing applies.
          pattern: isCheck || isArea ? null : (f.pattern || null),
        });
    if (isCheck) control.checked = f.default === "true";
    else control.value = f.default || "";
    if (helpId) control.setAttribute("aria-describedby", helpId);

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

  const defaultOf = (entry) => (entry.isCheck ? entry.field.default === "true"
                                              : entry.field.default || "");
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
      return Object.fromEntries(entries.map((e) =>
        [e.field.name, e.isCheck ? String(e.control.checked) : e.control.value]));
    },
  };
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

async function runAction(route, name, action, onDone, row = NO_ROW) {
  const apply = async () => {
    const op = recordOperation(`${action.label} ${name}`);
    row.start();
    try {
      const res = await send(
        `/api/actions/${route.service}?project=${encodeURIComponent(currentProject())}`,
        "POST", { Name: name, Action: action.id });
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

function stateOf(status) {
  if (!status) return "";
  const s = status.toLowerCase();
  if (["ready", "running", "enabled", "active", "succeeded", "true"].includes(s)) return "ok";
  if (["failed", "error", "destroyed"].includes(s)) return "error";
  if (["pending", "paused", "disabled", "unknown"].includes(s)) return "warn";
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

function dispatch(view) {
  markCurrent();

  const match = ROUTES.find((r) => r.path === location.pathname);
  document.title = match ? `${match.title} — CloudBurrow` : "CloudBurrow Console";

  stopStream();
  stopMetrics();
  METRICS_TICK = null;
  stopActivityPolling();
  REVEAL_SUSPENDED = false;
  if (!match) return notFound(view, location.pathname);
  if (match.screen === "search") return renderSearch(view);
  if (match.screen === "playground") return renderPlayground(view);
  if (match.screen === "logs") return renderLogs(view);
  if (match.screen === "activity") return renderActivity(view);
  if (match.screen === "create") return renderCreatePage(view, match);
  if (!match.service) return renderDashboard(view);
  const resource = new URLSearchParams(location.search).get("resource");
  if (resource) return renderDetail(view, match, resource);
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

// --- Logs Explorer ----------------------------------------------------
//
// Live by default, bounded, and pausable. A log view that cannot be paused is
// unusable the moment something interesting scrolls past.

let STREAM = null;
let METRICS_TIMER = null;

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
    if (!METRICS_TICK) return;
    if (document.hidden) {
      stopMetrics();
    } else if (!METRICS_TIMER) {
      METRICS_TICK();
      METRICS_TIMER = setInterval(METRICS_TICK, 5000);
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

async function renderLogs(view) {
  const params = new URLSearchParams(location.search);
  const project = params.get("project") || "";
  // Followed from a failed operation in Activity. The server has always
  // supported the filter; the client simply never read it, so the one path
  // built to explain a failure landed on the unfiltered stream of the whole
  // instance.
  let operation = params.get("operation") || "";

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
  const scope = el("div", { class: "filter-chips" });
  const body = el("tbody");
  const table = el("table", {},
    el("thead", {}, el("tr", {},
      ["Time", "Severity", "Source", "Message"].map((c) => el("th", { scope: "col", text: c })))),
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
  const drawEmpty = () => {
    if (rows) return;
    setChildren(body, el("tr", {},
      el("td", { colspan: "4" },
        el("div", { class: "state state-inline" },
          el("h2", { text: stalled || attempt
            ? "The log stream is not connected"
            : "No entries match these filters" }),
          el("p", { text: stalled || attempt
            ? "Nothing can be shown until the stream is back."
            : "Nothing has been logged that matches. Widen the filters, or wait." })))));
  };

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
    setChildren(scope, operation
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
    if (project) query.set("project", project);
    if (operation) query.set("operation", operation);
    if (severity.value) query.set("severity", severity.value);
    if (source.value) query.set("source", source.value);
    if (contains.value) query.set("contains", contains.value);
    query.set("limit", "200");

    setStatus(attempt ? `reconnecting (attempt ${attempt})` : "connecting…", "warn");

    const stream = new EventSource(`/api/stream?${query}`);
    STREAM = stream;
    stream.addEventListener("open", () => {
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
    el("div", { class: "actions" }, severity, source, contains,
       pauseButton, reconnectButton, status),
    scope,
    el("div", { class: "table-wrap" }, table));

  connect();
  announce(operation ? `Logs Explorer opened, scoped to operation ${operation}`
                     : "Logs Explorer opened");
}

// --- Activity ---------------------------------------------------------

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
  const listing = {
    nameColumn: "Operation",
    columns: ["Started", "Kind", "Resource", "Detail"],
    noun: "operations",
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

  const header = [
    pageHeader("Activity", "Operations this console performed."),
    running
      ? el("div", { class: "actions" },
          el("span", { class: "status is-working" },
            spinner(), el("span", { text: `${running} still running` })))
      : null,
  ];
  // No refetch: the running count in the header and the poll below are part
  // of this screen, so a refresh re-renders the screen rather than only its
  // rows. The table's Refresh button comes from the shared renderer.
  renderTableInto(view, header, listing, "operations",
    () => renderActivity(view), { path: "/activity", service: null, title: "Activity" }, {});
  announce(`${ops.length} operations`);

  // Polling stops the moment nothing is outstanding, so an idle screen costs
  // nothing. The path is checked as well as the timer, because a render
  // already in flight when the route changed would otherwise reschedule.
  stopActivityPolling();
  if (running && location.pathname === "/activity") {
    ACTIVITY_TIMER = setTimeout(() => renderActivity(view), ACTIVITY_POLL_MS);
  }
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
