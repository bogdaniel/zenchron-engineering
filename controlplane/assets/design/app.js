import { icon, esc, badge, panel, pageHead, link, emptyState } from "./components.js";

const preview = await Promise.all([
  import("./fixtures.js"),
  import("./overview.js"),
  import("./portfolio.js"),
  import("./workspace.js"),
  import("./actions.js"),
]);
const { fixture, objectives, agents, actionItems, scaleObjectives, timeline, scaleTimeline } = preview[0] || {};
const { overview } = preview[1] || {};
const { portfolio, updatePortfolio, readPortfolioSelection } = preview[2] || {};
const { workspace, workspaceTabs } = preview[3] || {};
const { actionInbox, decisionDialog, fixtureActionBlocker } = preview[4] || {};
const app = document.querySelector("#app");
const dialog = document.querySelector("#decision-dialog");
const resolved = new Map();
const mobileViewport = matchMedia("(max-width: 760px)");
let theme = "dark";
let scenario = "ready";
let inboxFilter = "All";
let menuOpen = false;
let lastDialogTrigger = null;
let decisionSnapshot = null;
let portfolioSelection = { query: "", state: "", product: "", offset: 0 };
let activityOffset = 0;
const objectiveSource = () => (scenario === "scale" ? scaleObjectives : objectives);

const navigation = [
  ["overview", "Engineering overview", "overview"],
  ["objectives", "Objectives", "objectives"],
  ["actions", "Action required", "actions"],
  ["products", "Products", "products"],
  ["agents", "Execution agents", "agents"],
  ["controller", "Controller health", "controller"],
];

function route() {
  const fragments = location.hash.replace(/^#\/?/, "").split("/");
  let id;
  try {
    id = decodeURIComponent(fragments[1] || "");
  } catch {
    return { page: "invalid", id: "", tab: "" };
  }
  return {
    page: fragments[0] || "overview",
    id,
    tab: fragments[2] || "lifecycle",
  };
}

function shell(content, active) {
  const endpoint =
    scenario === "disconnected"
      ? "Unreachable"
      : ["stale", "empty", "loading", "error"].includes(scenario)
        ? "Unobserved"
        : "Reachable";
  return `<aside class="sidebar ${menuOpen ? "is-open" : ""}" aria-label="Application sidebar"><button class="icon-button drawer-close" data-toggle-menu aria-label="Close navigation">${icon("close", 20)}</button><a class="brand" href="#/overview"><span class="brand-symbol" aria-hidden="true">Z<span>↗</span></span><span>ZENCHRON<small>ENGINEERING OS</small></span></a>
    <div class="local-workspace"><span class="workspace-mark">Z</span><span><strong>Local engineering</strong><small>One operator · one machine</small></span></div>
    <div class="eyebrow nav-label">COMMAND CENTER</div><nav aria-label="Primary navigation">${navigation.map(([destination, label, glyph]) => `<a href="#/${destination}" ${active === destination ? 'aria-current="page"' : ""}>${icon(glyph)}<span>${label}</span>${destination === "actions" ? `<span class="nav-count">${actionItems.length - resolved.size}</span>` : ""}</a>`).join("")}</nav>
    <div class="sidebar-bottom"><div class="sidebar-health">${`<span>Identity G42<small>Attested · simulated identity</small><small class="${endpoint === "Unreachable" ? "orange" : "secondary"}">Endpoint: ${endpoint}</small></span>`}</div><a href="#/diagnostics" class="support-link">${icon("help", 17)} Diagnostics & help</a><div class="sidebar-edition"><span>AUTHORIZATION KERNEL</span><span class="mono">LOCAL / 0.1</span></div></div></aside>
    <div class="application"><header class="topbar"><div class="topbar-location"><button class="icon-button menu-toggle" data-toggle-menu aria-label="Toggle navigation" aria-expanded="${menuOpen}">${icon("menu", 20)}</button><span class="topbar-product">${fixture.product}</span><span class="topbar-slash">/</span><span class="muted">Command center</span></div><div class="topbar-tools"><button class="search-trigger" data-open-search aria-label="${"Search objectives"}">${icon("search", 16)}<span>${"Find an objective"}</span><kbd>/</kbd></button><button class="icon-button" data-toggle-theme aria-label="Switch to ${theme === "dark" ? "light" : "dark"} theme">${icon("theme", 18)}</button><span class="operator-avatar" title="${"Fixture operator"}">OP</span></div></header>
    ${`<div class="preview-bar"><div><span class="fixture-label">DESIGN PREVIEW</span><span>Fixture data only</span><span class="preview-divider">/</span><span class="preview-note">No runtime measurements or actions</span></div><label class="scenario-control"><span>Preview state</span><select id="preview-state" aria-label="Preview state">${["ready", "empty", "loading", "error", "disconnected", "stale", "permission-refused", "failed-execution", "scale"].map((s) => `<option value="${s}" ${scenario === s ? "selected" : ""}>${s === "ready" ? "Connected fixture" : s[0].toUpperCase() + s.slice(1)}</option>`).join("")}</select></label></div>`}
    <main id="main" tabindex="-1">${content}</main><footer class="app-footer"><span><span class="status-dot ${scenario === "ready" ? "green" : "orange"}"></span> ${scenario === "ready" ? "FIXTURE SNAPSHOT" : "FIXTURE " + scenario.toUpperCase()}</span>${"<span>13 OCT 2026 · 14:32:08 UTC</span>"}<span>Evidence before authority.</span></footer>
    <nav class="mobile-nav" aria-label="Mobile navigation">${navigation
      .filter(([destination]) => ["overview", "objectives", "actions"].includes(destination))
      .map(
        ([destination, label, glyph]) =>
          `<a href="#/${destination}" ${active === destination ? 'aria-current="page"' : ""}>${icon(glyph, 19)}<span>${label === "Engineering overview" ? "Overview" : label === "Action required" ? "Decisions" : label}</span></a>`,
      )
      .join(
        "",
      )}<button data-toggle-menu aria-label="More navigation" aria-expanded="${menuOpen}">${icon("menu", 19)}<span>More</span></button></nav></div>`;
}

function stateContent(page) {
  if (["ready", "scale"].includes(scenario)) return "";
  if (["permission-refused", "failed-execution"].includes(scenario))
    return `<div class="banner orange connection-banner" role="status"><div><strong>${scenario === "permission-refused" ? "Permission refused · fixture controller response" : "Execution failed · fixture run-612-g1"}</strong><p>${scenario === "permission-refused" ? "The requested action is outside current authority. No elevated permission or retry is implied." : "Candidate retained; assurance and publication withheld. Inspect the causal failure; a failed plan is not restarted automatically."}</p></div>${link("Inspect context", scenario === "failed-execution" ? "objectives/OBJ-612/activity" : "controller", "button")}</div>`;
  if (scenario === "loading")
    return `${pageHead("PREVIEW / LOADING", "Reading engineering state", "Loading fixture presentation. No provider work is started.")}<section class="panel loading-state" aria-busy="true" aria-label="Loading engineering state"><div class="skeleton wide"></div><div class="skeleton short"></div><div class="skeleton-grid">${Array.from({ length: 6 }, () => '<div class="skeleton tall"></div>').join("")}</div><p role="status">Loading snapshot…</p><button class="button" data-recover>Complete fixture load</button></section>`;
  if (scenario === "error")
    return `${pageHead("PREVIEW / READ FAILURE", "Engineering state unavailable", "The snapshot could not be read. No missing facts are interpreted as success.")}<section class="panel state-panel">${icon("actions", 34)}<h2>We couldn’t load this view</h2><p class="muted">Fixture read_failed · no internal paths or diagnostic prose exposed.</p><div><button class="button primary" data-recover>Retry fixture read</button> ${link("Inspect diagnostics", "diagnostics", "button")}</div></section>`;
  if (scenario === "empty")
    return `${pageHead("PREVIEW / EMPTY WORKSPACE", page === "actions" ? "No decisions need attention" : "Your factory floor is ready", "No fixture work is visible in this scenario. Nothing starts implicitly.")}<section class="panel">${emptyState(page === "actions" ? "You’re up to date" : "No engineering objectives yet", page === "actions" ? "Human requests will appear here with their exact subject and consequence." : "Create work explicitly from an existing issue. Issue existence is never consent to execute.", '<button class="button primary" data-recover>Explore fixture objectives</button>')}</section>`;
  return `<div class="banner orange connection-banner" role="status">${icon("actions", 18)}<div><strong>${scenario === "stale" ? "Snapshot stale · last fixture observation 18 minutes ago" : "Controller disconnected · fixture scenario"}</strong><p>${scenario === "stale" ? "Retained data is historical. Refresh exact subjects before acting." : "Cached fixture data remains visible. Controller authority cannot be established."} Decisions are unavailable.</p></div><button class="button" data-recover>${scenario === "stale" ? "Refresh fixture" : "Reconnect fixture"}</button></div>`;
}

function supplemental(page) {
  if (page === "agents")
    return `${pageHead("SYSTEM / EXECUTION", "Execution agents", "Installed worker identity, provider kind, and execution trust are separate facts.")}<div class="supplemental-grid">${agents.map((a) => panel(a.id, `<dl class="binding-list summary-binding"><dt>Provider kind</dt><dd class="mono">${a.kind}</dd><dt>Trust mode</dt><dd class="mono">${a.trust}</dd><dt>Capacity</dt><dd>${a.used} executing runs · ceiling unavailable</dd><dt>Read confinement</dt><dd>${a.trust === "protected" ? "Proven boundary · fixture" : "Unproven"}</dd><dt>Acceptance authority</dt><dd>None</dd></dl><div class="panel-footer"><button class="button" data-inspect-agent="${a.id}">Inspect provenance</button></div>`, badge(a.state, a.used ? "green" : "neutral"))).join("")}</div>`;
  if (page === "products")
    return `${pageHead("OPERATIONS / PRODUCTS", "Products", "Product scope connects objectives, configuration, and operational WorkGraphs.")}<div class="supplemental-grid">${["Engineering OS", "Operator guides"].map((name, i) => panel(name, `<div class="product-body"><span class="eyebrow">PRODUCT REVISION ${i ? 2 : 6}</span><p>${i ? "Operator documentation in this local repository." : "The local engineering runtime and authorization kernel."}</p><dl class="binding-list"><dt>Repository</dt><dd class="mono">${fixture.repository}</dd><dt>Policy</dt><dd>${fixture.policy}</dd><dt>Objectives</dt><dd>${i ? "1" : "9"} fixture objectives</dd></dl>${link("Browse objectives", "objectives", "button")} ${link("Inspect WorkGraph", "objectives/OBJ-612/workgraph")}</div>`)).join("")}</div>`;
  return `${pageHead("SYSTEM / " + (page === "diagnostics" ? "DIAGNOSTICS" : "CONTROLLER"), page === "diagnostics" ? "Diagnostics & troubleshooting" : "Controller health", "Independent dimensions of the local controller. Every value below is a visual fixture.")}<div class="supplemental-grid">${panel("Controller identity", `<dl class="binding-list summary-binding"><dt>Generation</dt><dd class="mono">G42 · fixture-controller-g42</dd><dt>Durable consistency</dt><dd>${badge("Consistent", "green")}</dd><dt>Serving</dt><dd>${badge(scenario === "disconnected" ? "Unreachable" : "Serving", scenario === "disconnected" ? "orange" : "green")}</dd><dt>Role / admission</dt><dd>Held / open</dd><dt>Generation match</dt><dd>${scenario === "disconnected" ? "Unknown" : "Matches · fixture"}</dd><dt>Publication identity</dt><dd class="mono">engineering-runtime[bot]</dd><dt>Observed at</dt><dd class="mono">${fixture.observedAt}</dd></dl>`)}${panel("Governed operator actions", `<div class="product-body"><p>Production HTTP supports only stop and plan-reject today. The preview simulates decisions without submitting any action.</p><div class="banner neutral">Unknown or stale state never grants authority. An ambiguous mutation result must be reconciled before retrying.</div>${link("Review fixture decisions", "actions", "button")}</div>`)}${panel("Troubleshooting path", `<ol class="diagnostic-list"><li>Check controller reachability and generation match.</li><li>Inspect the exact run and its causal activity.</li><li>Distinguish current operation-row progress from journal history.</li><li>Read typed blockers, assurance lifecycle, and held material.</li><li>Use the existing CLI for capabilities outside the HTTP allowlist.</li></ol>`)}</div>`;
}

function render(focusMain = false) {
  const { page, id, tab } = route();
  let content = "";
  if (page === "overview") content = overview(resolved);
  else if (page === "objectives")
    content = id
      ? workspace(
          id,
          workspaceTabs.find((t) => t.toLowerCase() === tab) || "Lifecycle",
          scenario === "scale" ? scaleTimeline : timeline,
          activityOffset,
          scenario,
        )
      : portfolio(portfolioSelection, objectiveSource());
  else if (page === "actions") content = actionInbox(id, resolved, inboxFilter, scenario);
  else if (["products", "agents", "controller", "diagnostics"].includes(page)) content = supplemental(page);
  else
    content = `${pageHead("NAVIGATION / UNKNOWN", "View not found", "This prototype route is not available.", link("Return to overview", "overview", "button"))}`;
  const state = stateContent(page);
  if (["empty", "loading", "error"].includes(scenario)) content = state;
  else content = state + content;
  app.innerHTML = shell(content, page);
  syncNavigationFocus();
  document.documentElement.dataset.theme = theme;
  document.title = `${page === "overview" ? "Engineering overview" : page === "actions" ? "Action required" : page === "objectives" && id ? "Objective workspace" : page === "objectives" ? "Objectives portfolio" : "Zenchron"} · ${"Design preview"}`;
  if (["stale", "disconnected"].includes(scenario))
    app.querySelectorAll("[data-decision]").forEach((button) => (button.disabled = true));
  if (focusMain) document.querySelector("#main").focus({ preventScroll: true });
}

function syncNavigationFocus() {
  const drawer = document.querySelector(".sidebar");
  const isModal = mobileViewport.matches && menuOpen;
  drawer.inert = mobileViewport.matches && !menuOpen;
  if (isModal) {
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-modal", "true");
  } else {
    drawer.removeAttribute("role");
    drawer.removeAttribute("aria-modal");
  }
  document.querySelectorAll(".application > *").forEach((node) => (node.inert = isModal));
  if (drawer.inert && drawer.contains(document.activeElement)) document.querySelector(".menu-toggle").focus();
  if (isModal && !drawer.contains(document.activeElement)) drawer.querySelector("nav a").focus();
}
mobileViewport.addEventListener("change", syncNavigationFocus);

function announce(text) {
  document.querySelector("#announcer").textContent = text;
}

function openDialog(content, trigger) {
  lastDialogTrigger = trigger;
  dialog.innerHTML = content;
  dialog.showModal();
}

function closeDialog() {
  dialog.close();
  decisionSnapshot = null;
  lastDialogTrigger?.isConnected && lastDialogTrigger.focus();
}

document.addEventListener("click", (event) => {
  const button = event.target.closest("button");
  if (event.target.closest(".sidebar a, .mobile-nav a")) menuOpen = false;
  if (!button) return;
  if (button.hasAttribute("data-scroll-recovery")) {
    const recovery = document.querySelector("#runtime-recovery");
    recovery.scrollIntoView({ block: "start" });
    recovery.focus({ preventScroll: true });
    return;
  }
  if (button.hasAttribute("data-toggle-theme")) {
    theme = theme === "dark" ? "light" : "dark";
    render();
    document.querySelector("[data-toggle-theme]").focus();
    return;
  }
  if (button.hasAttribute("data-toggle-menu")) {
    menuOpen = !menuOpen;
    render();
    if (menuOpen) document.querySelector(".sidebar nav a").focus();
    else document.querySelector(".menu-toggle").focus();
    return;
  }
  if (button.hasAttribute("data-close-dialog")) {
    closeDialog();
    return;
  }
  if (button.hasAttribute("data-recover")) {
    scenario = "ready";
    render(true);
    announce("Fixture snapshot restored.");
    return;
  }
  if (button.id === "clear-filters" || button.id === "empty-reset") {
    for (const id of ["objective-search", "state-filter", "product-filter"]) document.getElementById(id).value = "";
    portfolioSelection = readPortfolioSelection();
    updatePortfolio(portfolioSelection, objectiveSource());
    document.querySelector("#objective-search").focus();
    return;
  }
  if (button.dataset.page) {
    const offset = Number(button.dataset.offset);
    if (!Number.isInteger(offset) || offset < 0) return;
    if (button.dataset.page === "portfolio") {
      portfolioSelection.offset = offset;
      updatePortfolio(portfolioSelection, objectiveSource());
    } else if (button.dataset.page === "activity") {
      activityOffset = offset;
      render();
    }
    document.querySelector(`[data-page="${button.dataset.page}"]:not(:disabled)`)?.focus();
    return;
  }
  if (button.dataset.decision) {
    if (
      fixtureActionBlocker(
        actionItems.find((item) => item.id === button.dataset.request),
        scenario,
      )
    )
      return;
    if (resolved.has(button.dataset.request)) return;
    const request = actionItems.find((item) => item.id === button.dataset.request);
    decisionSnapshot = Object.freeze({
      id: request.id,
      binding: request.binding,
      revision: request.revision,
      action: request.action,
      target: request.target,
      candidate: fixture.revision,
      tree: fixture.tree,
    });
    openDialog(decisionDialog(request.id, button.dataset.decision, scenario), button);
    return;
  }
  if (button.dataset.confirmDecision) {
    const request = actionItems.find((item) => item.id === button.dataset.request);
    if (
      !decisionSnapshot ||
      !request ||
      request.id !== decisionSnapshot.id ||
      request.binding !== decisionSnapshot.binding ||
      String(request.revision) !== button.dataset.revision ||
      request.revision !== decisionSnapshot.revision ||
      request.binding !== button.dataset.binding ||
      request.action !== decisionSnapshot.action ||
      request.target !== decisionSnapshot.target ||
      fixture.revision !== decisionSnapshot.candidate ||
      fixture.tree !== decisionSnapshot.tree
    ) {
      announce("Exact fixture subject changed. Reopen and inspect the request.");
      return;
    }
    if (
      fixtureActionBlocker(
        actionItems.find((item) => item.id === button.dataset.request),
        scenario,
      ) ||
      resolved.has(button.dataset.request)
    )
      return;
    resolved.set(button.dataset.request, {
      outcome: button.dataset.confirmDecision,
      note: document.querySelector("#decision-note")?.value.trim().slice(0, 500) || "",
      recordedAt: new Date().toISOString(),
    });
    closeDialog();
    render(true);
    document.querySelector("#decision-receipt")?.focus({ preventScroll: true });
    announce("Historical fixture receipt recorded. No runtime action was sent.");
    return;
  }
  if (button.hasAttribute("data-reset-decisions")) {
    resolved.clear();
    inboxFilter = "All";
    render(true);
    announce("Fixture decisions reset.");
    return;
  }
  if (button.dataset.inboxFilter) {
    inboxFilter = button.dataset.inboxFilter;
    render();
    document.querySelector(`[data-inbox-filter="${inboxFilter}"]`).focus();
    return;
  }
  if (button.dataset.inspectAgent) {
    const worker = agents.find((a) => a.id === button.dataset.inspectAgent);
    if (!worker) return;
    openDialog(
      `<div class="dialog-head"><span class="eyebrow">FIXTURE WORKER PROVENANCE</span><button class="icon-button" data-close-dialog aria-label="Close worker details">${icon("close", 20)}</button></div><h2 id="dialog-title">${worker.id}</h2><dl class="binding-list"><dt>Provider kind</dt><dd class="mono">${worker.kind}</dd><dt>Execution trust</dt><dd class="mono">${worker.trust}</dd><dt>Host read confinement</dt><dd>${worker.trust === "protected" ? "Proven · fixture" : "Unproven"}</dd><dt>Invocation mode</dt><dd>Bounded execution</dd><dt>Acceptance authority</dt><dd>None</dd><dt>Profile</dt><dd>Operator-defined specialization; may only narrow</dd></dl><div class="banner neutral">Worker completion is an observation. Independent evidence and action-scoped authority remain required.</div><button class="button" data-close-dialog>Close details</button>`,
      button,
    );
    return;
  }
  if (button.dataset.unit) {
    document.querySelectorAll("[data-unit]").forEach((node) => {
      node.classList.toggle("selected", node === button);
      node.setAttribute("aria-pressed", String(node === button));
    });
    const descriptions = {
      "WU-612-A": "Admitted handoff handoff-612-A-r1 · candidate 81e4b9a · codex-build",
      "WU-613-B": "Independent review handoff admitted · APPROVE for candidate 81e4b9a · claude-review",
      "WU-614-C": "Runnable frontier · issue #614 · waits for existing scheduler capacity",
      "WU-615-D": "Not runnable · admitted review and documentation handoffs required",
    };
    document.querySelector("#unit-detail").innerHTML =
      `<span class="mono orange">${button.dataset.unit}</span><span>${descriptions[button.dataset.unit]}</span><span class="muted small">An issue-backed WorkUnit is not a PlanStage.</span>`;
    return;
  }
  if (button.hasAttribute("data-open-search")) openSearch(button);
});

function openSearch(trigger) {
  openDialog(
    `<div class="dialog-head"><span class="eyebrow">QUICK NAVIGATION</span><button class="icon-button" data-close-dialog aria-label="Close search">${icon("close", 20)}</button></div><h2 id="dialog-title">Find an objective</h2><label for="global-search" class="note-label">Search fixture objectives</label><input id="global-search" type="search" placeholder="Title or issue number" autocomplete="off"><div id="global-search-results" class="global-search-results">${searchResults("")}</div>`,
    trigger,
  );
  document.querySelector("#global-search").focus();
}
function searchResults(query) {
  const items = objectiveSource().filter((o) =>
    `${o.id} ${o.title} ${o.issue}`.toLowerCase().includes(query.toLowerCase()),
  );
  return (
    items
      .slice(0, 6)
      .map(
        (o) =>
          `<a href="#/objectives/${o.id}" data-search-result><span class="mono muted small">${o.id}</span><strong>${esc(o.title)}</strong>${icon("arrow", 14)}</a>`,
      )
      .join("") || '<p class="muted" role="status">No fixture objectives match.</p>'
  );
}
dialog.addEventListener("click", (event) => {
  if (event.target.closest("[data-search-result]")) closeDialog();
});
dialog.addEventListener("cancel", () => {
  decisionSnapshot = null;
  lastDialogTrigger?.isConnected && lastDialogTrigger.focus();
});
document.addEventListener("input", (event) => {
  if (event.target.id === "objective-search") {
    portfolioSelection = readPortfolioSelection();
    updatePortfolio(portfolioSelection, objectiveSource());
  }
  if (event.target.id === "global-search")
    document.querySelector("#global-search-results").innerHTML = searchResults(event.target.value);
});
document.addEventListener("change", (event) => {
  if (["state-filter", "product-filter"].includes(event.target.id)) {
    portfolioSelection = readPortfolioSelection();
    updatePortfolio(portfolioSelection, objectiveSource());
  }
  if (event.target.id === "mobile-decision") location.hash = "#/actions/" + event.target.value;
  if (event.target.id === "preview-state") {
    scenario = event.target.value;
    portfolioSelection.offset = 0;
    activityOffset = 0;
    render();
    document.querySelector("#preview-state").focus();
  }
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Tab" && dialog.open) {
    const controls = [...dialog.querySelectorAll("button:not(:disabled), input, textarea, a[href], summary")].filter(
      (node) => node.getClientRects().length,
    );
    const first = controls[0];
    const last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
    return;
  }
  if (event.key === "Tab" && menuOpen && matchMedia("(max-width: 760px)").matches) {
    const controls = [...document.querySelectorAll(".sidebar a, .sidebar button")];
    const first = controls[0],
      last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
    return;
  }

  if (event.key === "Escape" && menuOpen) {
    menuOpen = false;
    render();
    document.querySelector(".menu-toggle").focus();
  }
  if (event.key !== "/" || /INPUT|TEXTAREA|SELECT/.test(event.target.tagName) || dialog.open) return;
  event.preventDefault();
  const search = document.querySelector("#objective-search");
  if (search) search.focus();
  else openSearch(document.querySelector("[data-open-search]"));
});
window.addEventListener("hashchange", () => {
  activityOffset = 0;
  render(true);
  window.scrollTo(0, 0);
});
render();
