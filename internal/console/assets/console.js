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

const ROUTES = [
  { path: "/",                      service: null,      title: "Dashboard" },
  { path: "/storage/browser",       service: "storage", title: "Buckets" },
  { path: "/pubsub/topics",         service: "pubsub",  title: "Topics" },
  { path: "/tasks/queues",          service: "tasks",   title: "Queues" },
  { path: "/run",                   service: "run",     title: "Services" },
  { path: "/secrets",               service: "secrets", title: "Secrets" },
  { path: "/kubernetes/workloads",  service: "workloads", title: "Workloads" },
  { path: "/kubernetes/pods",       service: "pods",      title: "Pods" },
  { path: "/kubernetes/services",   service: "k8sservices", title: "Kubernetes Services" },
  { path: "/kubernetes/jobs",       service: "jobs",      title: "Jobs" },
  { path: "/kubernetes/events",     service: "events",    title: "Events" },
  { path: "/ai/models",             service: "ai",        title: "Model catalogue" },
  { path: "/ai/playground",         service: "playground", screen: "playground", title: "AI Playground" },
  { path: "/logs",                  service: null, screen: "logs",       title: "Logs Explorer" },
  { path: "/activity",              service: null, screen: "activity",   title: "Activity" },
];

const ICONS = {
  playground: '<path d="M12 3a9 9 0 1 0 9 9"/><path d="M12 7v5l3 2"/><path d="M17 3l1.5 3L22 7.5 18.5 9 17 12l-1.5-3L12 7.5 15.5 6z"/>',
  storage:   '<path d="M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z"/><path d="M4 7v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  pubsub:    '<path d="M4 9h4l5-4v14l-5-4H4z"/><path d="M17 9a4 4 0 0 1 0 6"/>',
  tasks:     '<path d="M4 6h16M4 12h16M4 18h10"/><circle cx="19" cy="18" r="2"/>',
  run:       '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M9 10l4 2-4 2z"/>',
  secrets:   '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  pods:      '<circle cx="12" cy="12" r="8"/><path d="M12 8v8M8 12h8"/>',
  k8sservices: '<circle cx="12" cy="12" r="3"/><path d="M12 3v4M12 17v4M3 12h4M17 12h4"/>',
  jobs:      '<path d="M4 7h16v13H4z"/><path d="M9 7V4h6v3"/><path d="M9 13h6"/>',
  ai:        '<rect x="4" y="4" width="16" height="16" rx="3"/><circle cx="9" cy="10" r="1.4"/><circle cx="15" cy="10" r="1.4"/><path d="M9 15h6"/>',
  logs:      '<path d="M5 4h11l3 3v13H5z"/><path d="M8 11h8M8 15h5"/>',
  activity:  '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
  events:    '<circle cx="12" cy="12" r="9"/><path d="M12 7v6M12 16h.01"/>',
  workloads: '<rect x="3" y="4" width="7" height="7" rx="1"/><rect x="14" y="4" width="7" height="7" rx="1"/><rect x="3" y="13" width="7" height="7" rx="1"/><rect x="14" y="13" width="7" height="7" rx="1"/>',
  dashboard: '<rect x="3" y="3" width="8" height="10" rx="1"/><rect x="13" y="3" width="8" height="6" rx="1"/><rect x="3" y="15" width="8" height="6" rx="1"/><rect x="13" y="11" width="8" height="10" rx="1"/>',
};

// Capabilities come from the backend, so a control only ever appears when
// the service behind it can actually perform it.
let SERVICES = [];
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

  for (const entry of entries) {
    const icon = ICONS[entry.service] || ICONS.dashboard;
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
          el("span", { class: "nav-icon", html: `<svg viewBox="0 0 24 24" aria-hidden="true">${icon}</svg>` }),
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

function initNavToggle() {
  const toggle = document.getElementById("nav-toggle");
  const nav = document.getElementById("nav");
  toggle.addEventListener("click", () => {
    const open = nav.dataset.open === "true";
    nav.dataset.open = open ? "false" : "true";
    toggle.setAttribute("aria-expanded", open ? "false" : "true");
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

  setChildren(view, 
    el("h1", { text: "Dashboard" }),
    el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." }),
    cards,
    el("div", { class: "card", style: "margin-top:16px" },
      el("h2", { text: "Services" }),
      services.length
        ? el("ul", { style: "list-style:none;margin:0;padding:0" }, services)
        : el("p", { class: "unavailable", text: "No services are enabled." }))
  );
  announce("Dashboard loaded");
}

async function renderList(view, route) {
  const project = new URLSearchParams(location.search).get("project") || "";

  const header = [
    el("h1", { text: route.title }),
    el("p", { class: "subtitle",
              text: project ? `Project ${project}` : "All projects" }),
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
    const empty = emptyState(`No ${route.title.toLowerCase()} yet`,
      "Create one here, or with an SDK, the CLI or gcloud — it will appear either way.");
    if (caps.create) {
      empty.append(el("button", { class: "primary", text: caps.create.label,
        onclick: () => openCreateForm(route, caps.create, () => renderList(view, route)) }));
    }
    setChildren(view, ...header, empty);
    announce(`No ${route.title.toLowerCase()}`);
    return;
  }

  const filter = el("input", {
    class: "filter", type: "search", placeholder: `Filter ${route.title.toLowerCase()}`,
    "aria-label": `Filter ${route.title.toLowerCase()}`,
  });

  const caps = capabilityOf(route.service);
  const hasActions = data.items.some((i) => (i.actions || []).length) || caps.delete;
  const columns = [
    "Name",
    ...(data.columns || []),
    ...(data.items.some((i) => i.status) ? ["Status"] : []),
    ...(hasActions ? ["Actions"] : []),
  ];
  const body = el("tbody");
  const reload = () => renderList(view, route);

  const draw = (term) => {
    const q = term.trim().toLowerCase();
    const rows = data.items.filter((i) => !q || i.name.toLowerCase().includes(q));
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

  filter.addEventListener("input", () => draw(filter.value));
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
      el("table", {},
        el("thead", {}, el("tr", {}, columns.map((c) => el("th", { scope: "col", text: c })))),
        body)),
    el("p", { class: "subtitle", text: `${data.total} total` })
  );
  announce(`${data.items.length} ${route.title.toLowerCase()} loaded`);
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
      onDone();
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
  if (!match) return notFound(view, location.pathname);
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

async function initProjects() {
  const select = document.getElementById("project");
  const current = new URLSearchParams(location.search).get("project") || "";

  select.addEventListener("change", () => {
    const url = new URL(location.href);
    if (select.value) url.searchParams.set("project", select.value);
    else url.searchParams.delete("project");
    history.pushState({}, "", url);
    route();
  });

  // Projects are discovered from resources that exist, not from a list
  // CloudBurrow keeps: there is no project registry, and inventing one would
  // be a second store.
  const found = new Set();
  for (const r of ROUTES.filter((x) => x.service && !x.screen)) {
    try {
      const data = await api(`/api/resources/${r.service}`);
      for (const item of data.items || []) {
        const m = /^projects\/([^/]+)\//.exec(item.name);
        if (m) found.add(m[1]);
      }
    } catch { /* a service that cannot be read contributes no projects */ }
  }
  for (const p of [...found].sort()) {
    select.append(el("option", { value: p, text: p, selected: p === current }));
  }
  if (current && !found.has(current)) {
    select.append(el("option", { value: current, text: current, selected: true }));
  }
}

function initSearch() {
  const search = document.getElementById("search");
  search.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    const filter = document.querySelector(".filter");
    if (filter) {
      filter.value = search.value;
      filter.dispatchEvent(new Event("input"));
      filter.focus();
    }
  });
}

async function main() {
  initTheme();
  initPanel("settings", "settings-panel");
  initPanel("notifications", "notifications-panel");
  initNavToggle();
  initRouting();
  initSearch();

  try {
    SERVICES = (await api("/api/services")).services || [];
  } catch {
    // The navigation still renders the dashboard, which will show the error.
  }
  buildNav(SERVICES);
  route();
  initProjects();
}

document.addEventListener("DOMContentLoaded", main);

// --- Logs Explorer ----------------------------------------------------
//
// Live by default, bounded, and pausable. A log view that cannot be paused is
// unusable the moment something interesting scrolls past.

let STREAM = null;

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
