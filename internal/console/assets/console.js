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
];

const ICONS = {
  storage:   '<path d="M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z"/><path d="M4 7v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  pubsub:    '<path d="M4 9h4l5-4v14l-5-4H4z"/><path d="M17 9a4 4 0 0 1 0 6"/>',
  tasks:     '<path d="M4 6h16M4 12h16M4 18h10"/><circle cx="19" cy="18" r="2"/>',
  run:       '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M9 10l4 2-4 2z"/>',
  secrets:   '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  workloads: '<rect x="3" y="4" width="7" height="7" rx="1"/><rect x="14" y="4" width="7" height="7" rx="1"/><rect x="3" y="13" width="7" height="7" rx="1"/><rect x="14" y="13" width="7" height="7" rx="1"/>',
  dashboard: '<rect x="3" y="3" width="8" height="10" rx="1"/><rect x="13" y="3" width="8" height="6" rx="1"/><rect x="3" y="15" width="8" height="6" rx="1"/><rect x="13" y="11" width="8" height="10" rx="1"/>',
};

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

// --- data ------------------------------------------------------------

async function api(path) {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw new Error(`${path} responded ${res.status} ${res.statusText}`);
  }
  return res.json();
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
  const entries = [{ path: "/", service: "dashboard", title: "Dashboard" }].concat(
    ROUTES.filter((r) => r.service && available.has(r.service))
  );

  for (const entry of entries) {
    const icon = ICONS[entry.service] || ICONS.dashboard;
    list.append(
      el("li", {},
        el("a", { href: entry.path + location.search, "data-path": entry.path },
          el("span", { class: "nav-icon", html: `<svg viewBox="0 0 24 24" aria-hidden="true">${icon}</svg>` }),
          el("span", { text: entry.title })
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
  view.replaceChildren(
    el("h1", { text: "Dashboard" }),
    el("p", { class: "subtitle", text: "Live state of this CloudBurrow instance." }),
    loadingState(3)
  );

  let status;
  try {
    status = await api("/api/status");
  } catch (err) {
    view.replaceChildren(
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

  view.replaceChildren(
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
  view.replaceChildren(...header, loadingState());

  let data;
  try {
    data = await api(`/api/resources/${route.service}?project=${encodeURIComponent(project)}`);
  } catch (err) {
    view.replaceChildren(...header,
      errorState(`${route.title} unavailable`, String(err.message), () => renderList(view, route)));
    return;
  }

  // An unreachable backend is an error state, never an empty table: an empty
  // table says "you have none", which sends a developer to debug their code.
  if (data.unavailable) {
    view.replaceChildren(...header,
      errorState(`${route.title} unavailable`, data.unavailable, () => renderList(view, route)));
    announce(`${route.title} unavailable`);
    return;
  }

  if (!data.items.length) {
    view.replaceChildren(...header,
      emptyState(`No ${route.title.toLowerCase()} yet`,
        "Create one with an SDK, the CLI, or gcloud, and it will appear here."));
    announce(`No ${route.title.toLowerCase()}`);
    return;
  }

  const filter = el("input", {
    class: "filter", type: "search", placeholder: `Filter ${route.title.toLowerCase()}`,
    "aria-label": `Filter ${route.title.toLowerCase()}`,
  });

  const columns = ["Name", ...(data.columns || []), ...(data.items.some((i) => i.status) ? ["Status"] : [])];
  const body = el("tbody");

  const draw = (term) => {
    const q = term.trim().toLowerCase();
    const rows = data.items.filter((i) => !q || i.name.toLowerCase().includes(q));
    body.replaceChildren(...rows.map((item) =>
      el("tr", {},
        el("td", {}, item.link ? el("a", { href: item.link, text: item.name })
                               : document.createTextNode(item.name)),
        ...(data.columns || []).map((c) => el("td", { text: (item.fields || {})[c] || "—" })),
        ...(columns.includes("Status")
            ? [el("td", {}, el("span", { class: "status", "data-state": stateOf(item.status) },
                el("span", { text: item.status || "—" })))]
            : [])
      )));
    if (!rows.length) {
      body.replaceChildren(el("tr", {},
        el("td", { colspan: String(columns.length), class: "unavailable",
                   text: "No matches." })));
    }
  };

  filter.addEventListener("input", () => draw(filter.value));
  draw("");

  const note = data.note
    ? el("p", { class: "unavailable", text: data.note })
    : null;

  view.replaceChildren(...header, note,
    el("div", { class: "actions" },
      filter,
      el("button", { class: "secondary", text: "Refresh",
                     onclick: () => renderList(view, route) })),
    el("div", { class: "table-wrap" },
      el("table", {},
        el("thead", {}, el("tr", {}, columns.map((c) => el("th", { scope: "col", text: c })))),
        body)),
    el("p", { class: "subtitle", text: `${data.total} total` })
  );
  announce(`${data.items.length} ${route.title.toLowerCase()} loaded`);
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
  view.replaceChildren(
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

  if (!match) return notFound(view, location.pathname);
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
  for (const r of ROUTES.filter((x) => x.service)) {
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

  let services = [];
  try {
    services = (await api("/api/services")).services || [];
  } catch {
    // The navigation still renders the dashboard, which will show the error.
  }
  buildNav(services);
  route();
  initProjects();
}

document.addEventListener("DOMContentLoaded", main);
