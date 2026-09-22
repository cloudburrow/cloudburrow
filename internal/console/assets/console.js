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

  { path: "/run", service: "run", title: "Cloud Run", section: "Compute" },

  { path: "/kubernetes/workloads",  service: "workloads",   title: "Workloads", section: "Kubernetes Engine" },
  { path: "/kubernetes/pods",       service: "pods",        title: "Pods",      section: "Kubernetes Engine" },
  { path: "/kubernetes/services",   service: "k8sservices", title: "Services",  section: "Kubernetes Engine" },
  { path: "/kubernetes/jobs",       service: "jobs",        title: "Jobs",      section: "Kubernetes Engine" },
  { path: "/kubernetes/events",     service: "events",      title: "Events",    section: "Kubernetes Engine" },

  { path: "/storage/browser", service: "storage", title: "Cloud Storage", section: "Storage" },

  { path: "/firestore", service: "firestore", title: "Firestore", section: "Databases" },
  { path: "/datastore", service: "datastore", title: "Datastore", section: "Databases" },
  { path: "/bigtable",  service: "bigtable",  title: "Bigtable",  section: "Databases" },
  { path: "/spanner",   service: "spanner",   title: "Spanner",   section: "Databases" },

  { path: "/pubsub/topics", service: "pubsub", title: "Pub/Sub",     section: "Integration services" },
  { path: "/tasks/queues",  service: "tasks",  title: "Cloud Tasks", section: "Integration services" },

  { path: "/ai/models",     service: "ai",         title: "Vertex AI Model Garden", section: "AI and machine learning" },
  { path: "/ai/playground", service: "playground", screen: "playground",
    title: "Vertex AI Studio", section: "AI and machine learning" },

  { path: "/secrets", service: "secrets", title: "Secret Manager", section: "Security" },

  { path: "/logs",     service: null, screen: "logs",     title: "Logs Explorer", section: "Operations" },
  { path: "/activity", service: null, screen: "activity", title: "Activity",      section: "Operations" },

  { path: "/projects", service: "projects", title: "Resource Manager", section: "IAM and admin" },

  { path: "/search", service: null, screen: "search", title: "Search results" },
];

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
  "firestore", "datastore", "bigtable", "spanner",
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

async function api(path, options = {}) {
  const res = await fetch(path, {
    ...options,
    headers: { Accept: "application/json", ...(options.headers || {}) },
  });
  const text = await res.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch { /* not JSON */ }
  if (!res.ok) {
    // The service's own message reaches the screen. A status code alone
    // hides the constraint the caller actually violated.
    throw new Error(body.error || `${path} responded ${res.status} ${res.statusText}`);
  }
  return body;
}

const send = (path, method, body) =>
  api(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });

// --- operations ------------------------------------------------------
//
// Every mutation is recorded, in flight and on completion, so an operation
// never appears to have succeeded before the API said so.

const OPERATIONS = [];

function recordOperation(label) {
  const op = { label, state: "running", at: new Date() };
  OPERATIONS.unshift(op);
  renderOperations();
  return {
    succeeded(detail) { op.state = "succeeded"; op.detail = detail; renderOperations(); },
    failed(detail) { op.state = "failed"; op.detail = detail; renderOperations(); },
  };
}

function renderOperations() {
  const list = document.getElementById("notification-list");
  const count = document.getElementById("notification-count");
  const empty = document.querySelector("#notifications-panel .panel-empty");
  if (!list) return;

  setChildren(list, ...OPERATIONS.slice(0, 20).map((op) =>
    el("li", {},
      el("span", { class: "status", "data-state": op.state === "succeeded" ? "ok"
                    : op.state === "failed" ? "error" : "warn" },
        el("span", { text: op.label })),
      op.detail ? el("div", { class: "unavailable", text: op.detail }) : null)));

  const active = OPERATIONS.filter((o) => o.state === "running").length;
  if (empty) empty.hidden = OPERATIONS.length > 0;
  if (active > 0) { count.hidden = false; count.textContent = String(active); }
  else { count.hidden = true; }
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

function initPanel(buttonId, panelId) {
  const button = document.getElementById(buttonId);
  const panel = document.getElementById(panelId);

  const close = () => {
    panel.hidden = true;
    button.setAttribute("aria-expanded", "false");
  };
  const open = () => {
    panel.hidden = false;
    button.setAttribute("aria-expanded", "true");
    // Focus moves into the dialog, and Escape returns it: a dialog that
    // traps neither is one a keyboard user cannot leave.
    const first = panel.querySelector("input, button, a, select");
    if (first) first.focus();
  };

  button.addEventListener("click", () => (panel.hidden ? open() : (close(), button.focus())));
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { close(); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    if (!panel.hidden && !panel.contains(e.target) && !button.contains(e.target)) close();
  });
}

// --- navigation ------------------------------------------------------

function buildNav(services) {
  const list = document.getElementById("nav-list");
  list.replaceChildren();

  const available = new Set(services.map((s) => s.id));
  const entries = [{ path: "/", service: "dashboard", title: "Dashboard" }]
    .concat(ROUTES.filter((r) => r.service && available.has(r.service)))
    .concat(ROUTES.filter((r) => r.screen && !r.service).map((r) => ({ ...r, service: r.screen })));

  let section = null;
  for (const entry of entries) {
    // A section heading is emitted when the group changes, so an entry that is
    // filtered out cannot leave its heading behind with nothing under it.
    if (entry.section && entry.section !== section) {
      section = entry.section;
      list.append(el("li", { class: "nav-section", role: "presentation" },
        el("span", { text: section })));
    }
    // The published product icon when there is one, our own line art when
    // there is not. The <img> carries no alt text: the link beside it already
    // names the product, and repeating it announces everything twice.
    const mark = PRODUCT_ICONS.has(entry.service)
      ? el("img", { class: "nav-icon-img", src: `/icons/${entry.service}.svg`, alt: "",
                    width: "20", height: "20", loading: "lazy" })
      : el("span", { html: `<svg viewBox="0 0 24 24" aria-hidden="true">${ICONS[entry.service] || ICONS.dashboard}</svg>` });
    list.append(
      el("li", {},
        // The label is hidden in the collapsed rail, so the name has to come
        // from somewhere: without this a narrow window leaves every link
        // announced as "link" and nothing else. title also gives the rail the
        // hover tooltip a collapsed navigation needs to be usable.
        el("a", {
          href: entry.path + location.search, "data-path": entry.path,
          "aria-label": entry.title, title: entry.title,
        },
          el("span", { class: "nav-icon" }, mark),
          el("span", { class: "nav-label", text: entry.title })
        )
      )
    );
  }
  markCurrent();
}

function markCurrent() {
  for (const a of document.querySelectorAll("#nav a")) {
    if (a.dataset.path === location.pathname) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// The menu button collapses and expands the navigation at every width.
//
// It used to set an attribute that only one media query read, so above 960px
// the button was visible, focusable, announced as a menu control — and did
// nothing at all. Now it drives the rail directly: expanded shows labels,
// collapsed shows icons, and below 960px the rail is a drawer that slides in.
const NAV_STATE_KEY = "cloudburrow.nav";

function navIsDrawer() {
  return window.matchMedia("(max-width: 959px)").matches;
}

function applyNavState(expanded) {
  const nav = document.getElementById("nav");
  const toggle = document.getElementById("nav-toggle");
  document.documentElement.dataset.nav = expanded ? "expanded" : "collapsed";
  nav.dataset.open = expanded ? "true" : "false";
  toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
}

function initNavToggle() {
  const toggle = document.getElementById("nav-toggle");

  // Remembered per browser, because a developer who collapses the rail wants
  // it collapsed on the next page too. A drawer always starts closed: one
  // that reopened itself on every load would cover the content.
  let expanded;
  if (navIsDrawer()) {
    expanded = false;
  } else {
    let stored = null;
    try { stored = localStorage.getItem(NAV_STATE_KEY); } catch { /* private mode */ }
    // Wide windows start expanded; narrow ones start as a rail, which is what
    // the width itself suggests.
    expanded = stored === null
      ? window.matchMedia("(min-width: 1280px)").matches
      : stored === "expanded";
  }
  applyNavState(expanded);

  toggle.addEventListener("click", () => {
    const now = document.documentElement.dataset.nav !== "expanded";
    applyNavState(now);
    if (!navIsDrawer()) {
      try { localStorage.setItem(NAV_STATE_KEY, now ? "expanded" : "collapsed"); } catch { /* ignore */ }
    }
  });

  // A drawer that stays open after a link is followed hides the page the link
  // just opened.
  document.getElementById("nav").addEventListener("click", (e) => {
    if (navIsDrawer() && e.target.closest("a")) applyNavState(false);
  });
}

// --- states ----------------------------------------------------------
//
// Four distinct states, because each is separately easy to get wrong and an
// error rendered as an empty table is the one that costs a developer an hour.

const loadingState = (rows = 5) =>
  el("div", { class: "skeleton", "aria-label": "Loading", role: "status" },
    Array.from({ length: rows }, () => el("div")));

const emptyState = (title, hint) =>
  el("div", { class: "state" },
    el("h2", { text: title }),
    el("p", { text: hint }));

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

function renderMetrics(target, m) {
  if (m.unavailable) {
    setChildren(target,
      el("h2", { text: "Cluster utilisation" }),
      el("p", { class: "unavailable", text: m.unavailable }));
    return;
  }
  const collected = m.collected ? new Date(m.collected).toLocaleTimeString() : "";
  setChildren(target,
    el("h2", { text: "Cluster utilisation" }),
    ...(m.nodes || []).flatMap((n) => [
      el("div", { class: "meter-node" },
        el("span", { class: "mono", text: n.name }),
        el("span", { class: "status", "data-state": n.ready ? "ok" : "error" },
          el("span", { text: n.ready ? "Ready" : "Not ready" })),
        el("span", { class: "unavailable", text: `${n.pods} pods` })),
      meterRow("CPU", n.cpuUsedCores || 0, n.cpuCapacityCores || 0, formatCores),
      meterRow("Memory", n.memoryUsedBytes || 0, n.memoryTotalBytes || 0, formatBytes),
    ]),
    el("p", { class: "panel-empty", text: collected ? `Updated ${collected}` : "" }));
}

async function renderDashboard(view) {
  setChildren(view, 
    el("h1", { text: "Dashboard" }),
    el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." }),
    loadingState(3)
  );

  let status;
  try {
    status = await api("/api/status");
  } catch (err) {
    setChildren(view, 
      el("h1", { text: "Dashboard" }),
      errorState("Instance status unavailable", String(err.message), () => renderDashboard(view))
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

  announce("Dashboard loaded");
}

async function renderList(view, route) {
  const project = new URLSearchParams(location.search).get("project") || "";

  const header = [
    el("div", { class: "page-header" },
      el("h1", { text: route.title }),
      el("p", { class: "subtitle",
                text: project ? `Project ${project}` : "All projects" })),
  ];
  setChildren(view, ...header, loadingState());

  let data;
  try {
    data = await api(`/api/resources/${route.service}?project=${encodeURIComponent(project)}`);
  } catch (err) {
    setChildren(view, ...header,
      errorState(`${route.title} unavailable`, String(err.message), () => renderList(view, route)));
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
        onclick: () => openCreateForm(route, caps.create, () => renderList(view, route)) }));
    }
    setChildren(view, ...header, empty);
    announce(`No ${noun}`);
    return;
  }

  const filter = el("input", {
    class: "filter", type: "search", placeholder: `Filter ${noun}`,
    "aria-label": `Filter ${noun}`,
  });

  const caps = capabilityOf(route.service);
  const hasActions = data.items.some((i) => (i.actions || []).length) || caps.delete;
  const columns = [
    data.nameColumn || "Name",
    ...(data.columns || []),
    ...(data.items.some((i) => i.status) ? ["Status"] : []),
    ...(hasActions ? ["Actions"] : []),
  ];
  const body = el("tbody");
  const reload = () => renderList(view, route);

  // Sorting state. A table of any length is unusable without it, and the
  // default is the order the service returned, which is meaningful often
  // enough that it should not be silently replaced.
  let sortColumn = null;
  let sortAscending = true;

  const valueOf = (item, column) => {
    if (column === (data.nameColumn || "Name")) return item.name || "";
    if (column === "Status") return item.status || "";
    return (item.fields || {})[column] || "";
  };

  const draw = (term) => {
    const q = term.trim().toLowerCase();
    let rows = data.items.filter((i) => !q || i.name.toLowerCase().includes(q));

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
    setChildren(body, ...rows.map((item) =>
      el("tr", {},
        el("td", {}, item.link ? el("a", { href: item.link, text: item.name })
                               : document.createTextNode(item.name)),
        ...(data.columns || []).map((c) => el("td", { text: (item.fields || {})[c] || "—" })),
        ...(columns.includes("Status")
            ? [el("td", {}, el("span", { class: "status", "data-state": stateOf(item.status) },
                el("span", { text: item.status || "—" })))]
            : []),
        ...(hasActions
            ? [el("td", { class: "row-actions" },
                ...(item.actions || []).map((a) =>
                  el("button", { class: "secondary", text: a.label,
                    onclick: () => runAction(route, item.name, a, reload) })),
                caps.delete
                  ? el("button", { class: "secondary", text: "Delete",
                      onclick: () => deleteResource(route, item.name, reload) })
                  : null)]
            : [])
      )));
    if (!rows.length) {
      setChildren(body, el("tr", {},
        el("td", { colspan: String(columns.length), class: "unavailable",
                   text: "No matches." })));
    }
  };

  const headRow = el("tr");
  const drawHead = () => {
    setChildren(headRow, ...columns.map((c) => {
      // Actions is a column of controls, not of values, so it does not sort:
      // offering it would be a control that does nothing.
      if (c === "Actions") return el("th", { scope: "col", text: c });
      const active = sortColumn === c;
      const arrow = active ? (sortAscending ? "\u2191" : "\u2193") : "";
      return el("th", {
        scope: "col",
        "aria-sort": active ? (sortAscending ? "ascending" : "descending") : "none",
      },
        el("button", {
          class: "sort-button" + (active ? " is-active" : ""),
          onclick: () => {
            if (sortColumn === c) sortAscending = !sortAscending;
            else { sortColumn = c; sortAscending = true; }
            drawHead();
            draw(filter.value);
            announce(`Sorted by ${c}, ${sortAscending ? "ascending" : "descending"}`);
          },
        },
          el("span", { text: c }),
          el("span", { class: "sort-arrow", "aria-hidden": "true", text: arrow })));
    }));
  };

  filter.addEventListener("input", () => draw(filter.value));
  drawHead();
  draw("");

  const note = data.note
    ? el("p", { class: "unavailable", text: data.note })
    : null;

  setChildren(view, ...header, note,
    el("div", { class: "actions" },
      // The create button exists only when the backend says the service can
      // create: an unsupported operation is absent, not disabled.
      caps.create
        ? el("button", { class: "primary", text: caps.create.label,
            onclick: () => openCreateForm(route, caps.create, reload) })
        : null,
      filter,
      el("button", { class: "secondary", text: "Refresh", onclick: reload })),
    el("div", { class: "table-wrap" },
      el("table", {}, el("thead", {}, headRow), body)),
    el("p", { class: "subtitle", text: `${data.total} total` })
  );
  announce(`${data.items.length} ${noun} loaded`);
}

// --- create form ------------------------------------------------------
//
// The form is described by the backend, so a field only appears when the
// service behind it can accept it.

function openCreateForm(route, spec, onDone) {
  const project = new URLSearchParams(location.search).get("project") || "";

  const dialog = el("div", { class: "modal", role: "dialog", "aria-modal": "true",
                             "aria-labelledby": "create-title" });
  const error = el("p", { class: "form-error", role: "alert", hidden: true });

  const inputs = spec.fields.map((f) => {
    const input = el("input", {
      id: `f-${f.name}`, name: f.name, type: f.type || "text",
      required: f.required, pattern: f.pattern || null, value: f.default || "",
      "aria-describedby": f.help ? `h-${f.name}` : null,
    });
    return { field: f, input,
      node: el("div", { class: "form-row" },
        el("label", { for: `f-${f.name}`, text: f.label }),
        input,
        f.help ? el("p", { id: `h-${f.name}`, class: "form-help", text: f.help }) : null) };
  });

  const close = () => { dialog.remove(); document.getElementById("main").focus(); };

  const submit = async (e) => {
    e.preventDefault();
    error.hidden = true;

    // Validated before submission against the same constraint the API
    // enforces, so an obvious mistake does not need a round trip.
    for (const { field, input } of inputs) {
      if (!input.checkValidity()) {
        error.textContent = `${field.label} is not valid. ${field.help || ""}`.trim();
        error.hidden = false;
        input.focus();
        return;
      }
    }

    const values = Object.fromEntries(inputs.map(({ field, input }) => [field.name, input.value]));
    const op = recordOperation(`${spec.label} in ${route.title}`);
    for (const b of dialog.querySelectorAll("button")) b.disabled = true;

    try {
      const res = await send(
        `/api/resources/${route.service}?project=${encodeURIComponent(project)}`,
        "POST", values);
      op.succeeded(res.name);
      announce(`Created ${res.name}`);
      close();
      // The name is passed on so a caller can act on what was just made —
      // the project picker selects it. Existing callers ignore it.
      onDone(res.name);
    } catch (err) {
      op.failed(err.message);
      error.textContent = err.message;
      error.hidden = false;
      for (const b of dialog.querySelectorAll("button")) b.disabled = false;
    }
  };

  const form = el("form", { class: "modal-body", onsubmit: submit },
    el("h2", { id: "create-title", text: spec.label }),
    error,
    ...inputs.map((i) => i.node),
    el("div", { class: "modal-actions" },
      el("button", { type: "button", class: "secondary", text: "Cancel", onclick: close }),
      el("button", { type: "submit", class: "primary", text: spec.label })));

  dialog.append(form);
  dialog.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });
  dialog.addEventListener("click", (e) => { if (e.target === dialog) close(); });
  document.body.append(dialog);
  if (inputs.length) inputs[0].input.focus();
}

// A destructive action names exactly what it will affect: "are you sure"
// with no subject is how the wrong resource gets deleted.
async function confirmDestructive(verb, name) {
  return window.confirm(`${verb} ${name}?\n\nThis cannot be undone.`);
}

async function deleteResource(route, name, onDone) {
  if (!(await confirmDestructive("Delete", name))) return;
  const project = new URLSearchParams(location.search).get("project") || "";
  const op = recordOperation(`Delete ${name}`);
  try {
    await send(`/api/resources/${route.service}?project=${encodeURIComponent(project)}` +
      `&name=${encodeURIComponent(name)}`, "DELETE");
    op.succeeded();
    announce(`Deleted ${name}`);
    onDone();
  } catch (err) {
    op.failed(err.message);
    announce(`Delete failed: ${err.message}`);
    window.alert(`Could not delete ${name}:\n\n${err.message}`);
  }
}

async function runAction(route, name, action, onDone) {
  if (action.destructive && !(await confirmDestructive(action.label, name))) return;
  const project = new URLSearchParams(location.search).get("project") || "";
  const op = recordOperation(`${action.label} ${name}`);
  try {
    await send(`/api/actions/${route.service}?project=${encodeURIComponent(project)}`,
      "POST", { Name: name, Action: action.id });
    op.succeeded();
    announce(`${action.label} applied to ${name}`);
    onDone();
  } catch (err) {
    op.failed(err.message);
    announce(`${action.label} failed: ${err.message}`);
    window.alert(`${action.label} failed for ${name}:\n\n${err.message}`);
  }
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

function route() {
  const view = document.getElementById("view");
  markCurrent();

  const match = ROUTES.find((r) => r.path === location.pathname);
  document.title = match ? `${match.title} — CloudBurrow` : "CloudBurrow Console";

  stopStream();
  stopMetrics();
  if (!match) return notFound(view, location.pathname);
  if (match.screen === "search") return renderSearch(view);
  if (match.screen === "playground") return renderPlayground(view);
  if (match.screen === "logs") return renderLogs(view);
  if (match.screen === "activity") return renderActivity(view);
  if (!match.service) return renderDashboard(view);
  return renderList(view, match);
}

function initRouting() {
  document.addEventListener("click", (e) => {
    const link = e.target.closest("a[href]");
    if (!link) return;
    const url = new URL(link.href, location.origin);
    if (url.origin !== location.origin) return;
    e.preventDefault();
    history.pushState({}, "", url);
    route();
    document.getElementById("main").focus();
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

  const select = (id) => {
    const url = new URL(location.href);
    if (id) url.searchParams.set("project", id);
    else url.searchParams.delete("project");
    history.pushState({}, "", url);
    selected = id;
    current.textContent = id || "All projects";
    panel.hidden = true;
    button.setAttribute("aria-expanded", "false");
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
    if (panel.hidden) {
      await load();
      panel.hidden = false;
      button.setAttribute("aria-expanded", "true");
      filter.focus();
    } else {
      panel.hidden = true;
      button.setAttribute("aria-expanded", "false");
    }
  });
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { panel.hidden = true; button.setAttribute("aria-expanded", "false"); button.focus(); }
  });
  document.addEventListener("click", (e) => {
    if (!panel.hidden && !panel.contains(e.target) && !button.contains(e.target)) {
      panel.hidden = true;
      button.setAttribute("aria-expanded", "false");
    }
  });

  newButton.addEventListener("click", () => {
    panel.hidden = true;
    button.setAttribute("aria-expanded", "false");
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

  const pathFor = (service) => {
    const r = ROUTES.find((x) => x.service === service);
    return r ? r.path + location.search.replace(/[?&]q=[^&]*/, "").replace(/^&/, "?") : "/";
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
            el("a", { href: pathFor(h.service), class: "search-hit" },
              el("span", { class: "search-hit-name", text: h.name }),
              h.detail ? el("span", { class: "search-hit-detail", text: h.detail }) : null,
              h.status
                ? el("span", { class: "status", "data-state": stateOf(h.status) },
                    el("span", { text: h.status }))
                : null))))));

  const failed = Object.entries(data.failed || {});
  setChildren(view, header,
    el("p", { class: "subtitle",
              text: `${(data.hits || []).length} result(s) across ${data.searched} service(s)` +
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
    blocks.length
      ? el("div", { class: "cards" }, blocks)
      : el("div", { class: "state" },
          el("h2", { text: "No matches" }),
          el("p", { text: `Nothing matching “${q}” in the services that answered.` })));

  announce(`${(data.hits || []).length} search results`);
}

async function main() {
  initTheme();
  initPanel("settings", "settings-panel");
  initPanel("account", "account-panel");
  initPanel("notifications", "notifications-panel");
  initNavToggle();
  initRouting();
  initSearch();

  try {
    SERVICES = (await api("/api/services")).services || [];
  } catch {
    // The navigation still renders the dashboard, which will show the error.
  }
  try {
    DEFAULT_PROJECT = (await api("/api/status")).defaultProject || "";
  } catch {
    // Without it the picker simply opens on "All projects", as before.
  }
  buildNav(SERVICES);
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

function stopMetrics() {
  if (METRICS_TIMER) { clearInterval(METRICS_TIMER); METRICS_TIMER = null; }
}

function stopStream() {
  if (STREAM) { STREAM.close(); STREAM = null; }
}

const MAX_RENDERED_LINES = 1000;

async function renderLogs(view) {
  const params = new URLSearchParams(location.search);
  const project = params.get("project") || "";

  const severity = el("select", { id: "severity", "aria-label": "Minimum severity" },
    ...["", "INFO", "WARNING", "ERROR"].map((v) =>
      el("option", { value: v, text: v || "All severities" })));
  const source = el("input", { class: "filter", type: "search", id: "source",
    placeholder: "Source, e.g. run/my-service", "aria-label": "Filter by source" });
  const contains = el("input", { class: "filter", type: "search", id: "contains",
    placeholder: "Message contains", "aria-label": "Filter by message text" });

  const pauseButton = el("button", { class: "secondary", text: "Pause" });
  const status = el("span", { class: "unavailable", text: "connecting…" });
  const body = el("tbody");
  const table = el("table", {},
    el("thead", {}, el("tr", {},
      ["Time", "Severity", "Source", "Message"].map((c) => el("th", { scope: "col", text: c })))),
    body);

  let paused = false;
  let buffered = [];

  const append = (entry) => {
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
    while (body.childElementCount > MAX_RENDERED_LINES) body.firstElementChild.remove();
  };

  pauseButton.addEventListener("click", () => {
    paused = !paused;
    pauseButton.textContent = paused ? "Resume" : "Pause";
    status.textContent = paused ? `paused — ${buffered.length} buffered` : "live";
    if (!paused) {
      // Resume shows what happened while paused rather than skipping it:
      // the lines you paused to read are usually next to the ones you need.
      for (const e of buffered) append(e);
      buffered = [];
      announce("Log stream resumed");
    } else {
      announce("Log stream paused");
    }
  });

  const connect = () => {
    stopStream();
    body.replaceChildren();
    const query = new URLSearchParams();
    if (project) query.set("project", project);
    if (severity.value) query.set("severity", severity.value);
    if (source.value) query.set("source", source.value);
    if (contains.value) query.set("contains", contains.value);
    query.set("limit", "200");

    const stream = new EventSource(`/api/stream?${query}`);
    STREAM = stream;
    stream.addEventListener("open", () => {
      status.textContent = paused ? "paused" : "live";
    });
    stream.addEventListener("log", (e) => {
      let entry;
      try { entry = JSON.parse(e.data); } catch { return; }
      if (paused) {
        buffered.push(entry);
        if (buffered.length > MAX_RENDERED_LINES) buffered.shift();
        status.textContent = `paused — ${buffered.length} buffered`;
        return;
      }
      append(entry);
    });
    stream.addEventListener("error", () => {
      // EventSource reconnects on its own and resumes from Last-Event-ID,
      // so this reports rather than rebuilds.
      status.textContent = "reconnecting…";
    });
  };

  for (const control of [severity, source, contains]) {
    control.addEventListener("change", connect);
  }

  setChildren(view, 
    el("h1", { text: "Logs Explorer" }),
    el("p", { class: "subtitle",
      text: "Live from the local stack. Credentials are redacted before an entry is stored." }),
    el("div", { class: "actions" }, severity, source, contains, pauseButton, status),
    el("div", { class: "table-wrap" }, table));

  connect();
  announce("Logs Explorer opened");
}

// --- Activity ---------------------------------------------------------

async function renderActivity(view) {
  const project = new URLSearchParams(location.search).get("project") || "";
  setChildren(view, 
    el("h1", { text: "Activity" }),
    el("p", { class: "subtitle", text: "Operations this console performed." }),
    loadingState(4));

  let data;
  try {
    data = await api(`/api/operations?project=${encodeURIComponent(project)}`);
  } catch (err) {
    setChildren(view, el("h1", { text: "Activity" }),
      errorState("Activity unavailable", String(err.message), () => renderActivity(view)));
    return;
  }

  const ops = data.operations || [];
  if (!ops.length) {
    setChildren(view, 
      el("h1", { text: "Activity" }),
      emptyState("No operations yet",
        "Create or delete something in the console and it will appear here."));
    return;
  }

  const body = el("tbody", {}, ...ops.map((op) => {
    const row = el("tr", {},
      el("td", { class: "mono", text: new Date(op.started).toLocaleTimeString() }),
      el("td", { text: op.kind }),
      el("td", { class: "mono", text: op.resource }),
      el("td", {}, el("span", { class: "status",
        "data-state": op.state === "SUCCEEDED" ? "ok"
          : op.state === "FAILED" ? "error" : "warn" },
        el("span", { text: op.state }))),
      el("td", {},
        // A failed operation links to its own logs: "it failed" without the
        // reason is the least useful thing a console can say.
        op.state === "FAILED"
          ? el("a", { href: `/logs?operation=${encodeURIComponent(op.id)}`,
                      text: op.error || "see logs" })
          : document.createTextNode(op.error || "—")));
    return row;
  }));

  setChildren(view, 
    el("h1", { text: "Activity" }),
    el("p", { class: "subtitle", text: "Operations this console performed." }),
    el("div", { class: "actions" },
      el("button", { class: "secondary", text: "Refresh", onclick: () => renderActivity(view) })),
    el("div", { class: "table-wrap" },
      el("table", {},
        el("thead", {}, el("tr", {},
          ["Started", "Kind", "Resource", "State", "Detail"].map((c) =>
            el("th", { scope: "col", text: c })))),
        body)));
  announce(`${ops.length} operations`);
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

const PLAYGROUND_HISTORY_LIMIT = 20;
let PLAYGROUND_HISTORY = [];
let PLAYGROUND_ABORT = null;

function stopGeneration() {
  if (PLAYGROUND_ABORT) { PLAYGROUND_ABORT.abort(); PLAYGROUND_ABORT = null; }
}

async function renderPlayground(view) {
  stopGeneration();
  setChildren(view, el("h1", { text: "AI Playground" }), loadingState(3));

  let status;
  try {
    status = await api("/api/ai/playground");
  } catch (err) {
    return setChildren(view, 
      el("h1", { text: "AI Playground" }),
      errorState("Playground unavailable", String(err.message), () => renderPlayground(view)));
  }

  if (!status.configured) {
    return setChildren(view, 
      el("h1", { text: "AI Playground" }),
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
    el("h1", { text: "AI Playground" }),
    header,
    refused,
    el("div", { class: "card" },
      prompt,
      el("div", { class: "toolbar" }, send, cancel, timing),
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
