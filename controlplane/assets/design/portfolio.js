import { objectives, actionItems } from "./fixtures.js";
import { icon, pageHead, objectiveRows, emptyState, link, esc, pageWindow, pageControls } from "./components.js";

export function filterObjectives(query = "", state = "", product = "", source = objectives) {
  const words = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  return source.filter(
    (item) =>
      (!state || item.state === state) &&
      (!product || item.product === product) &&
      words.every((word) =>
        `${item.id} ${item.title} ${item.detail} ${item.issue} ${item.product} ${item.blocker}`
          .toLowerCase()
          .includes(word),
      ),
  );
}

function results(selection, source) {
  const items = filterObjectives(selection.query, selection.state, selection.product, source);
  const page = pageWindow(items, selection.offset);
  return {
    page,
    content: page.items.length
      ? objectiveRows(page.items)
      : emptyState(
          "No objectives match these filters",
          "Try a shorter search or clear these filters.",
          '<button class="button" id="empty-reset">Clear filters</button>',
        ),
  };
}

export function portfolio(selection = { query: "", state: "", product: "", offset: 0 }, source = objectives) {
  const { page, content } = results(selection, source);
  return `${pageHead("OPERATIONS / OBJECTIVES", "Objectives portfolio", "Observed milestones and exact next boundaries. Counts do not measure remaining effort.", link("Review pending plans", "actions", "button"))}
    <div class="portfolio-summary"><div><span class="mono">${source.length}</span> fixture objectives</div><div><span class="status-dot orange"></span> ${actionItems.length} human requests</div><div class="muted">Activities can overlap</div></div>
    <section class="panel portfolio-panel" aria-label="Search and filter objectives"><div class="filter-bar">
      <label class="search-input">${icon("search", 17)}<span class="sr-only">Search objectives</span><input id="objective-search" type="search" value="${esc(selection.query)}" placeholder="Search titles, issue numbers, or blockers…" autocomplete="off"><kbd aria-hidden="true">/</kbd></label>
      <label><span class="sr-only">Lifecycle filter</span><select id="state-filter"><option value="">All activity states</option>${["Executing", "Planning", "Review", "Decision", "Remediation", "Queued", "Accepted"].map((state) => `<option ${selection.state === state ? "selected" : ""}>${state}</option>`).join("")}</select></label>
      <label><span class="sr-only">Product filter</span><select id="product-filter"><option value="">All products</option>${["Engineering OS", "Operator guides"].map((product) => `<option ${selection.product === product ? "selected" : ""}>${product}</option>`).join("")}</select></label>
      <button class="button subtle" id="clear-filters">Reset</button>
    </div><div id="portfolio-results">${content}</div><footer class="table-footer"><span id="result-count" role="status">${page.total} objectives · fixture</span><div id="portfolio-pages">${pageControls("portfolio", page)}</div></footer></section>
    <div class="portfolio-notes"><div><span class="eyebrow">MILESTONES ≠ COMPLETION</span><p class="muted">Admitted handoff and exact-review counts describe established outcomes. They are never weighted into an overall progress percentage.</p></div><div><span class="eyebrow">RECOVERY HAS AN OWNER</span><p class="muted">Human requests go to the decision inbox. Bounded runtime recovery and dependency waits stay in the objective workspace.</p></div></div>`;
}

export function readPortfolioSelection() {
  return {
    query: document.querySelector("#objective-search").value,
    state: document.querySelector("#state-filter").value,
    product: document.querySelector("#product-filter").value,
    offset: 0,
  };
}

export function updatePortfolio(selection, source = objectives) {
  const { page, content } = results(selection, source);
  document.querySelector("#portfolio-results").innerHTML = content;
  document.querySelector("#result-count").textContent = `${page.total} objectives · fixture`;
  document.querySelector("#portfolio-pages").innerHTML = pageControls("portfolio", page);
}
